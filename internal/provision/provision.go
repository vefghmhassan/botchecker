// Package provision reacts to an address that has been blocked from Iran
// repeatedly: it alerts, optionally creates a replacement server from a
// Hetzner snapshot, and verifies the new address from inside Iran.
package provision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vefgh/botchecker/internal/checkhost"
	"github.com/vefgh/botchecker/internal/config"
	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/notify"
	"github.com/vefgh/botchecker/internal/scanner"
	"github.com/vefgh/botchecker/internal/settings"
	"github.com/vefgh/botchecker/internal/store"
	"github.com/vefgh/botchecker/internal/xui"
	"github.com/vefgh/botchecker/internal/zex"
)

// ErrBusy is returned when a provisioning run is already in flight.
var ErrBusy = errors.New("a provisioning run is already in progress")

type Manager struct {
	cfg     *config.Config
	set     *settings.Provider
	st      *store.Store
	ch      *checkhost.Client
	hz      *hetzner.Registry
	panel   *xui.Client
	zex     *zex.Client
	tg      *notify.Telegram
	handoff Handoff
	log     *slog.Logger

	irNodes map[string]bool

	mu      sync.Mutex
	running bool

	prog progressLog
}

func New(cfg *config.Config, set *settings.Provider, st *store.Store, ch *checkhost.Client,
	hz *hetzner.Registry, panel *xui.Client, zexClient *zex.Client, tg *notify.Telegram,
	handoff Handoff, log *slog.Logger) *Manager {

	ir := make(map[string]bool, len(cfg.IRNodes))
	for _, n := range cfg.IRNodes {
		ir[n] = true
	}
	return &Manager{
		cfg: cfg, set: set, st: st, ch: ch, hz: hz, panel: panel, zex: zexClient,
		tg: tg, handoff: handoff, log: log, irNodes: ir,
	}
}

func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// Trigger reacts to one machine crossing the outage threshold. It runs
// synchronously; callers that must not block should start it in a goroutine.
//
// It takes the whole machine rather than the port whose counter happened to
// cross. Everything this leads to — hiding the configs, buying a replacement,
// moving the configs across — acts on the address and cannot be pointed at a
// single port, so deciding from one port's evidence meant the other ports were
// carried along without ever being consulted.
func (m *Manager) Trigger(ctx context.Context, addr store.AddressStatus, outageCount int) error {
	return m.TriggerWith(ctx, addr, outageCount, Override{})
}

// Override relaxes the guards that exist to stop the service acting on its own
// too eagerly. It is for a human who has looked at the machine and decided.
//
// Deliberately narrow. The cooldown and the fully-blocked rule both encode
// "wait, you might be wrong", and a person pressing the button has already
// answered that. Everything else stays: an address outside the Hetzner projects
// still cannot be replaced, a machine marked hands-off is still left alone, and
// dry run still stops short of touching anything — that one is a safety net
// rather than a hesitation, and a button that ignored it would make the setting
// a lie.
type Override struct {
	// Cooldown lets a replacement start inside the quiet period after the last
	// one. Without it a manual attempt is usually answered with a silent
	// "skipped", which is the opposite of what pressing the button means.
	Cooldown bool
	// FullBlock lets a machine be replaced when Iran can still reach some of
	// it — a partial block, or a server that is simply down.
	FullBlock bool
	// Manual marks a run a person started. "Create servers automatically" is
	// about what the service does on its own, so switching it off must not
	// also disable the button; and the floating-IP rescue is allowed, because
	// someone asking for this now wants every way of getting there tried.
	Manual bool
}

// TriggerWith is Trigger with the guards a person may waive.
func (m *Manager) TriggerWith(ctx context.Context, addr store.AddressStatus, outageCount int, ov Override) error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return ErrBusy
	}
	m.running = true
	m.mu.Unlock()
	m.beginProgress(addr.Address)

	return m.runHeld(ctx, addr, outageCount, ov)
}

func (m *Manager) run(ctx context.Context, addr store.AddressStatus, outageCount int, ov Override) error {
	now := time.Now()
	if len(addr.Ports) == 0 {
		return fmt.Errorf("no ports known for %s", addr.Address)
	}
	// The worst-reading port stands for the machine in the records and the
	// alert, so what the operator is shown is the most broken evidence rather
	// than whichever port sorted first.
	target := addr.Trigger()

	// An address this service has already destroyed can still be in the config
	// list, so it keeps being probed, keeps failing, and keeps asking to be
	// replaced. On 2026-09-21 that produced nine attempts for one address and
	// seven for another, none of which could ever succeed.
	//
	// It is not silently dropped: the configs still point at a dead machine, and
	// nobody would know. It reports once a day and then stays quiet.
	if burned, entry, err := m.st.IsBurned(addr.Address); err != nil {
		m.log.Warn("could not read the address ledger", "address", addr.Address, "err", err)
	} else if burned {
		m.step(StepSkipped, "this address is in the ledger as already replaced or deleted")
		return m.refuseBurned(addr, entry, outageCount, now)
	}

	// The cooldown is checked before anything else, including the alert.
	// Otherwise a blocked address would re-alert every time the threshold is
	// reached — roughly every three watch rounds — which is pure noise for a
	// block the operator already knows about.
	if within, since := m.withinCooldown(addr.Address); within && !ov.Cooldown {
		m.log.Info("inside the cooldown, staying quiet",
			"address", addr.Address, "last_trigger", since.Round(time.Minute))
		id, err := m.st.CreateProvision(target.TargetID, "", m.dryRun(), outageCount, nil, now)
		if err != nil {
			return err
		}
		m.step(StepSkipped, fmt.Sprintf("cooldown: last attempt was %s ago", since.Round(time.Minute)))
		return m.st.SetProvisionStatus(id, store.ProvSkipped, true,
			fmt.Errorf("cooldown: last trigger was %s ago", since.Round(time.Minute)), time.Now())
	}

	online := m.onlineCount(ctx, addr.Address)
	owner, resource, project := m.ownership(ctx, addr.Address)

	alert := alertContext{
		Target:      target,
		Ports:       addr.PortNumbers(),
		NotifyOnly:  addr.NotifyOnly(),
		OutageCount: outageCount,
		Window:      m.set.Duration(settings.OutageWindow, m.cfg.OutageWindow).String(),
		Online:      online,
		Provider:    string(owner),
	}
	if resource != nil {
		alert.ServerName = resource.ServerName
		alert.AbuseBlock = resource.AbuseBlocked
	}
	if tr, err := m.st.LatestTraceroute(target.TargetID); err == nil {
		alert.Traceroute = tr
	}

	provisionID, err := m.st.CreateProvision(target.TargetID, string(owner),
		m.dryRun(), outageCount, online, now)
	if err != nil {
		return err
	}

	// An address outside the Hetzner project can never be replaced by this
	// service. Saying so loudly matters more than staying quiet: most blocked
	// addresses in the real config are not Hetzner's.
	if owner == hetzner.OwnedUnknown {
		// Saying "not yours" here would be a guess, and the operator would act
		// on it. Stop and say plainly that ownership could not be established.
		m.log.Warn("ownership unknown, not acting", "address", addr.Address)
		m.step(StepSkipped, "could not determine which Hetzner project owns this address")
		return m.st.SetProvisionStatus(provisionID, store.ProvSkipped, true,
			fmt.Errorf("skipped: could not determine which Hetzner project owns %s", addr.Address),
			time.Now())
	}
	if owner != hetzner.OwnedHetzner {
		m.step(StepSkipped, "this address is not in any of your Hetzner projects")
		m.send(ctx, KindBlockedExternal, target.TargetID, provisionID, externalMessage(alert))
		return m.st.SetProvisionStatus(provisionID, store.ProvExternal, true, nil, time.Now())
	}
	_ = project

	m.send(ctx, KindBlocked, target.TargetID, provisionID, blockedMessage(alert))

	// An endpoint the operator marked hands-off still alerts, but is never
	// replaced — this is the "tell me, do not touch it" case.
	if addr.NotifyOnly() {
		m.log.Info("machine is marked notify-only, not creating a server", "address", addr.Address)
		m.step(StepSkipped, "this machine is marked alert-only")
		return m.st.SetProvisionStatus(provisionID, store.ProvNotifyOnly, true, nil, time.Now())
	}

	// "Only touch the ones Iran cannot reach at all." A partial block still
	// carries users on whichever network still works, and replacing it would
	// cut them off to fix something that is only half broken.
	//
	// The question is asked of the whole machine. Quieting and replacing act on
	// the address, so one port reading 0/8 while its sibling still answers is
	// not a blocked machine — it is a machine that would be taken away from the
	// users the sibling is still serving.
	if m.set.Bool(settings.ProvisionFullBlockOnly) && !ov.FullBlock && !addr.FullyBlocked() {
		open, total := addr.Reachable()
		m.log.Info("not replacing a partially reachable machine",
			"address", addr.Address, "ports", addr.PortNumbers(),
			"ir_open", open, "ir_total", total)
		m.step(StepSkipped, fmt.Sprintf("still reachable from %d of %d Iranian networks", open, total))
		return m.st.SetProvisionStatus(provisionID, store.ProvSkipped, true,
			fmt.Errorf("skipped: %s is still reachable from %d of %d Iranian networks",
				addr.Address, open, total), time.Now())
	}

	if reason := m.blockedFromProvisioning(ov); reason != "" {
		m.log.Info("not creating a server", "target", target.HostPort(), "reason", reason)
		m.step(StepSkipped, reason)
		return m.st.SetProvisionStatus(provisionID, store.ProvSkipped, true,
			fmt.Errorf("skipped: %s", reason), time.Now())
	}

	if m.dryRun() {
		m.log.Info("dry run: stopping before creating a server", "target", target.HostPort())
		m.step(StepDryRun)
		return m.st.SetProvisionStatus(provisionID, store.ProvDryRun, true, nil, time.Now())
	}

	// With the panel wired up the whole cycle runs: quiet the configs, build
	// somewhere healthy, verify, then move every config across. Without it the
	// service can only create a server and report the address.
	if m.zex != nil && m.zex.Configured() {
		return m.swap(ctx, addr, provisionID, ov)
	}
	return m.createAndVerify(ctx, addr, provisionID)
}

// createAndVerify is the path taken when the panel is not configured: buy a
// replacement, prove it works from Iran, and report the address for the
// operator to wire in by hand.
//
// It shares build with the full swap rather than creating servers its own way,
// so both paths pick projects, locations and server types by the same rules.
func (m *Manager) createAndVerify(ctx context.Context, addr store.AddressStatus, provisionID int64) error {
	if err := m.st.SetProvisionStatus(provisionID, store.ProvCreating, false, nil, time.Now()); err != nil {
		return err
	}

	candidates, skipped := m.buildCandidates()
	if len(candidates) == 0 {
		reason := "no Hetzner project is configured to build in"
		if len(skipped) > 0 {
			reason += ": " + strings.Join(skipped, ", ")
		}
		return m.fail(provisionID, errors.New(reason))
	}

	candidates, noImage := m.withSnapshots(ctx, candidates)
	if len(candidates) == 0 {
		return m.fail(provisionID, errors.New("no project has a snapshot to build from: "+strings.Join(noImage, ", ")))
	}

	// build announces the server and waits for it to boot, so neither is
	// repeated here. It used to be: the alert went out twice and the boot wait
	// was served twice, adding a minute and a half to every replacement.
	_, address, err := m.build(ctx, addr, provisionID, candidates)
	if err != nil {
		return m.fail(provisionID, err)
	}

	return m.verify(ctx, addr, provisionID, address)
}

// verify probes the new address from Iran, on every port the machine served.
//
// It delegates to verifyAddress so there is one implementation of "is this
// replacement usable" rather than two that can drift apart.
func (m *Manager) verify(ctx context.Context, addr store.AddressStatus, provisionID int64, address string) error {
	verdict, byPort, err := m.verifyAddress(ctx, address, addr.PortNumbers(), provisionID)
	if err != nil {
		return err
	}

	healthy := verdict == scanner.VerdictHealthy
	m.send(ctx, KindVerified, addr.Trigger().TargetID, provisionID,
		verifiedMessage(addr.Address, address, describePorts(byPort), string(verdict), healthy))

	if !healthy {
		m.step(StepUnhealthy, address, string(verdict), describePorts(byPort))
		// A brand new address can arrive already blocked. Reporting it and
		// stopping is deliberate: retrying would loop through paid servers.
		return m.st.SetProvisionStatus(provisionID, store.ProvVerifyFail, true, nil, time.Now())
	}

	trigger := addr.Trigger()
	if err := m.handoff.OnServerReady(ctx, ProvisionedServer{
		ReplacesAddress: addr.Address,
		Ports:           addr.PortNumbers(),
		ConfigIDs:       trigger.ConfigIDs,
		Names:           trigger.Names,
		Address:         address,
		SnapshotID:      m.set.Get(settings.HetznerSnapshotID),
		VerifiedAt:      time.Now(),
	}); err != nil {
		m.log.Error("handoff failed", "err", err)
		if serr := m.st.SetProvisionHandoff(provisionID, "failed: "+err.Error()); serr != nil {
			m.log.Warn("could not record handoff failure", "err", serr)
		}
	} else if serr := m.st.SetProvisionHandoff(provisionID, store.HandoffPending); serr != nil {
		m.log.Warn("could not record handoff state", "err", serr)
	}

	m.step(StepHealthy, address, describePorts(byPort))
	m.progressNewAddress(address)
	return m.st.SetProvisionStatus(provisionID, store.ProvVerified, true, nil, time.Now())
}

// ---- guards ----

// withinCooldown reports whether this address triggered recently. Records that
// were themselves skipped do not count, so a quiet period cannot extend itself.
func (m *Manager) withinCooldown(address string) (bool, time.Duration) {
	cooldown := m.set.Duration(settings.ProvisionCooldown, m.cfg.ProvisionCooldown)
	last, err := m.st.LastProvisionAt(address)
	if err != nil || last == nil {
		return false, 0
	}
	since := time.Since(*last)
	return since < cooldown, since
}

// blockedFromProvisioning returns a human reason why no server may be created,
// or "" when it may proceed. The cooldown is not checked here — it is applied
// before the alert, in run.
func (m *Manager) blockedFromProvisioning(ov Override) string {
	if !m.set.Bool(settings.ProvisionEnabled) && !ov.Manual {
		return "automatic provisioning is switched off"
	}
	if !m.hz.Configured() {
		return "no Hetzner token is configured"
	}

	limit := m.set.Int(settings.ProvisionMaxPerDay, m.cfg.ProvisionMaxPerDay)
	if n, err := m.st.ServersCreatedSince(time.Now().Add(-24 * time.Hour)); err == nil && n >= limit {
		return fmt.Sprintf("daily limit reached (%d servers in the last 24h)", n)
	}
	return ""
}

func (m *Manager) dryRun() bool { return m.set.Bool(settings.ProvisionDryRun) }

// ---- helpers ----

// onlineCount asks the panel how many users are connected to this address.
// A nil result means the panel could not answer, which is reported in the
// alert rather than being shown as zero.
func (m *Manager) onlineCount(ctx context.Context, address string) *int {
	if !m.panel.Configured() {
		return nil
	}
	n, found, err := m.panel.OnlineFor(ctx, address)
	if err != nil {
		m.log.Warn("could not read the online count from the panel", "err", err)
		return nil
	}
	if !found {
		return nil
	}
	return &n
}

// ownership classifies the address across every configured project, and says
// which one holds it.
//
// An error is never reported as "external". A project that could not be read is
// not evidence that the address belongs to someone else, and the external
// branch tells the operator nothing will be done automatically — a claim this
// has no business making on a failed lookup.
func (m *Manager) ownership(ctx context.Context, address string) (hetzner.Ownership, *hetzner.Resource, string) {
	if m.hz == nil || !m.hz.Configured() {
		return hetzner.OwnedUnknown, nil, ""
	}

	located, err := m.hz.Locate(ctx, address)
	if err != nil {
		m.log.Warn("could not determine which project owns the address",
			"address", address, "err", err)
		return hetzner.OwnedUnknown, nil, ""
	}
	if located.Ownership != hetzner.OwnedHetzner {
		return located.Ownership, nil, ""
	}
	res := located.Resource
	return hetzner.OwnedHetzner, &res, located.Project
}

// send delivers an alert and records the attempt. A delivery failure never
// stops the run — the work matters more than the notification.
func (m *Manager) send(ctx context.Context, kind string, targetID, provisionID int64, message string) {
	err := m.tg.Send(ctx, message)
	switch {
	case errors.Is(err, notify.ErrNotConfigured):
		m.log.Warn("telegram is not configured, alert not sent", "kind", kind)
	case err != nil:
		m.log.Error("could not send the telegram alert", "kind", kind, "err", err)
	default:
		m.log.Info("alert sent", "kind", kind)
	}

	if rerr := m.st.RecordNotification(kind, targetID, provisionID, err == nil, err, message, time.Now()); rerr != nil {
		m.log.Warn("could not record the notification", "err", rerr)
	}
}

func (m *Manager) fail(provisionID int64, err error) error {
	m.log.Error("provisioning failed", "provision_id", provisionID, "err", err)
	if serr := m.st.SetProvisionStatus(provisionID, store.ProvFailed, true, err, time.Now()); serr != nil {
		m.log.Warn("could not record the failure", "err", serr)
	}
	return err
}

func splitList(s string) []string {
	var out []string
	for _, part := range splitComma(s) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
