package provision

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/scanner"
	"github.com/vefgh/botchecker/internal/settings"
	"github.com/vefgh/botchecker/internal/store"
	"github.com/vefgh/botchecker/internal/zex"
)

// The rescue path: when no project can build, give the blocked machine a new
// address instead of a new machine.
//
// Every project here is capped at five servers, and on 2026-09-21 all of them
// were full. A replacement could not be built, so a machine whose configs had
// already been switched off stayed off with nothing to move them to — users
// offline, and nothing the service could do about it.
//
// What is actually wrong is the address, not the machine: the disk, the certs
// and the configs are all fine. A floating IP changes only the address, needs
// no slot, and costs about a euro a month rather than twenty.

// ErrNoFloatingRescue means the rescue was not attempted, with the reason.
var ErrNoFloatingRescue = errors.New("floating rescue unavailable")

// floatResult carries what the caller needs to finish the swap.
type floatResult struct {
	Address  string
	ServerID int64
	Project  string
	ID       int64
}

// rescueWithFloatingIP gives the blocked machine a new address and proves it
// works from Iran. It returns the new address on success.
//
// It stops short of moving the configs: the caller owns that step for the
// ordinary replacement too, and having one place that talks to the panel is
// what keeps the two paths from drifting apart.
func (m *Manager) rescueWithFloatingIP(ctx context.Context, addr store.AddressStatus,
	provisionID int64, ov Override) (*floatResult, error) {

	if !m.set.Bool(settings.ProvisionFloatOnFailure) && !ov.Manual {
		return nil, fmt.Errorf("%w: the setting is off", ErrNoFloatingRescue)
	}

	// The machine must still exist and must still be a server in a project we
	// hold a token for — the same three checks that stand in front of deleting
	// one, for the same reason: a server id means nothing outside its project.
	project, err := m.st.AddressProject(addr.Address)
	if err != nil || project == "" {
		return nil, fmt.Errorf("%w: no project is recorded for %s", ErrNoFloatingRescue, addr.Address)
	}
	cli, ok := m.hz.Client(project)
	if !ok {
		return nil, fmt.Errorf("%w: project %s is gone from the configuration", ErrNoFloatingRescue, project)
	}
	inv, err := cli.Inventory(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: could not read project %s: %v", ErrNoFloatingRescue, project, err)
	}
	res, ok := inv.Lookup(addr.Address)
	if !ok || res.ServerID == 0 {
		return nil, fmt.Errorf("%w: %s is not a server in %s", ErrNoFloatingRescue, addr.Address, project)
	}

	location := res.Location
	if location == "" {
		return nil, fmt.Errorf("%w: %s has no location", ErrNoFloatingRescue, addr.Address)
	}

	m.step(StepFloatTry, location)
	ip, err := m.reserveCleanFloatingIP(ctx, cli, addr.Address, location)
	if err != nil {
		return nil, err
	}

	// From here on the floating IP exists and is billed, so every failure path
	// gives it back.
	release := func(why string) {
		m.log.Warn("giving the floating address back", "address", ip.Address, "why", why)
		free := context.WithoutCancel(ctx)
		if err := cli.UnassignFloatingIP(free, ip.ID); err != nil {
			m.log.Warn("could not unassign the floating address", "id", ip.ID, "err", err)
		}
		if err := cli.DeleteFloatingIP(free, ip.ID); err != nil {
			m.log.Error("could not delete the floating address — delete it by hand",
				"address", ip.Address, "id", ip.ID, "err", err)
		}
	}

	if _, err := cli.AssignFloatingIP(ctx, ip.ID, res.ServerID); err != nil {
		release("assignment failed")
		return nil, fmt.Errorf("could not assign %s to server %d: %w", ip.Address, res.ServerID, err)
	}
	m.log.Info("floating address assigned",
		"address", ip.Address, "server_id", res.ServerID, "project", project)

	// Hetzner routes the address to the machine but does not put it on the
	// interface. Until something does, every probe reads as a dead host — which
	// is indistinguishable from a filtered address, and would get a perfectly
	// good IP condemned.
	configured := false
	if m.sshConfigured() {
		if err := m.configureFloatingIP(ctx, addr.Address, ip.Address); err != nil {
			m.log.Error("could not configure the floating address on the machine", "err", err)
		} else {
			configured = true
		}
	}
	if !configured {
		m.send(ctx, KindFloatManual, addr.Trigger().TargetID, provisionID,
			floatManualMessage(addr.Address, ip.Address, res.ServerID, floatingIPScript(ip.Address)))
		release("the address could not be configured inside the machine")
		return nil, fmt.Errorf("%w: %s is assigned but not configured on the machine, "+
			"and no SSH key is set for me to do it", ErrNoFloatingRescue, ip.Address)
	}

	verdict, byPort, err := m.verifyAddress(ctx, ip.Address, addr.PortNumbers(), provisionID)
	if err != nil {
		// The probe failed, which says nothing about the address. The floating
		// IP is kept and reported rather than silently destroyed.
		return nil, err
	}
	healthy := verdict == scanner.VerdictHealthy
	m.send(ctx, KindVerified, addr.Trigger().TargetID, provisionID,
		verifiedMessage(addr.Address, ip.Address, describePorts(byPort), string(verdict), healthy))

	if !healthy {
		m.step(StepUnhealthy, ip.Address, string(verdict), describePorts(byPort))
		// Burned only because the configuration was confirmed applied. Without
		// that confirmation this reading would be about the command that never
		// ran, not about the address, and a usable IP would be lost for good.
		m.burn(ip.Address, store.BurnDiscarded, store.LedgerEntry{
			Verdict:     string(verdict),
			Project:     project,
			ServerID:    res.ServerID,
			ProvisionID: provisionID,
			Note:        "floated onto " + addr.Address + " and came back " + describePorts(byPort),
		})
		release("it came back " + string(verdict) + " from Iran")
		return nil, fmt.Errorf("%s came back %s from Iran (%s)",
			ip.Address, verdict, describePorts(byPort))
	}

	m.step(StepFloatReady, ip.Address)
	return &floatResult{Address: ip.Address, ServerID: res.ServerID, Project: project, ID: ip.ID}, nil
}

// reserveCleanFloatingIP asks for addresses until one is not already known to
// be dead. Created unassigned on purpose: the address is readable before it is
// pointed at anything, so a recycled one costs a create and a delete rather
// than a reconfiguration.
func (m *Manager) reserveCleanFloatingIP(ctx context.Context, cli *hetzner.Client,
	replacing, location string) (*hetzner.FloatingIP, error) {

	attempts := m.set.Int(settings.ProvisionIPAttempts, defaultIPAttempts)
	if attempts < 1 {
		attempts = 1
	}

	var rejected []string
	for i := 1; i <= attempts; i++ {
		desc := fmt.Sprintf("botchecker rescue for %s (%s)", replacing,
			time.Now().UTC().Format("2006-01-02 15:04"))
		ip, err := cli.CreateFloatingIP(ctx, desc, location)
		if err != nil {
			return nil, fmt.Errorf("reserve a floating address in %s: %w", location, err)
		}

		why := m.dirtyAddress(ip.Address)
		if why == "" {
			if len(rejected) > 0 {
				m.log.Info("reserved a clean floating address after refusing recycled ones",
					"address", ip.Address, "refused", rejected)
			}
			return ip, nil
		}

		m.log.Info("the floating pool offered an address that is already dead, asking for another",
			"address", ip.Address, "why", why, "attempt", i)
		rejected = append(rejected, ip.Address)
		if derr := cli.DeleteFloatingIP(context.WithoutCancel(ctx), ip.ID); derr != nil {
			m.log.Error("could not release a rejected floating address — delete it by hand",
				"address", ip.Address, "id", ip.ID, "err", derr)
		}
	}
	return nil, fmt.Errorf("%w: %s kept offering dead addresses (%v)",
		ErrNoCleanAddress, location, rejected)
}

// finishFloatingSwap moves the configs onto the rescued address.
//
// Identical to the end of an ordinary replacement, and deliberately so: the
// panel is told an old address and a new one, and nothing about how the new one
// came to exist. The country does not change — it is the same machine in the
// same datacenter, only reachable by a different number.
func (m *Manager) finishFloatingSwap(ctx context.Context, addr store.AddressStatus,
	provisionID int64, r *floatResult, configs int) error {

	m.step(StepMoving, configs, addr.Address, r.Address)
	result, err := m.zex.ReplaceAddress(ctx, zex.ReplaceRequest{
		OldAddress: addr.Address,
		NewAddress: r.Address,
		Country:    addr.CountryCode,
		Activate:   true,
	})
	if err != nil {
		return m.abandon(ctx, addr, provisionID, configs,
			fmt.Errorf("the floating address works but the panel swap failed: %w", err))
	}
	m.step(StepMoved, result.Updated, result.Skipped)
	m.progressNewAddress(r.Address)

	// The machine is still here and still ours; only the number changed. So the
	// old address is recorded as dead — it is still filtered and must never be
	// handed out again — while the server is not touched.
	m.burn(addr.Address, store.BurnRetired, store.LedgerEntry{
		Verdict:     addr.Trigger().Verdict,
		Project:     r.Project,
		ServerID:    r.ServerID,
		ProvisionID: provisionID,
		Note:        "replaced by floating address " + r.Address + " on the same machine",
	})

	if err := m.st.SetProvisionStatus(provisionID, store.ProvSwapped, true, nil, time.Now()); err != nil {
		m.log.Warn("could not record the swap", "err", err)
	}
	m.send(ctx, KindFloated, addr.Trigger().TargetID, provisionID,
		floatedMessage(addr.Address, r.Address, r.ServerID, result.Updated))

	m.log.Info("rescued with a floating address",
		"was", addr.Address, "now", r.Address, "server_id", r.ServerID,
		"configs", result.Updated, "skipped", result.Skipped)
	return nil
}
