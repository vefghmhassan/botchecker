package provision

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/scanner"
	"github.com/vefgh/botchecker/internal/settings"
	"github.com/vefgh/botchecker/internal/store"
	"github.com/vefgh/botchecker/internal/zex"
)

// swap runs the full replacement: build somewhere healthy, prove the new
// address works from Iran, then move every config onto it in one go.
//
// The configs are deliberately left serving until the replacement is proved.
// They used to be switched off first, and the cost of that was measured on the
// one replacement that has run end to end: users were offline for 389 seconds,
// of which 327 were spent building and verifying while their configs pointed at
// nothing. Switching off early bought nothing — the machine being replaced
// answers from no Iranian network, so a config pointing at it is already dead —
// and it turned a few seconds of cutover into six and a half minutes of outage.
//
// The safety property is kept, on the path where it actually applies: if
// nothing usable can be built, abandon switches them off, because then there is
// no new address to move to and a config that is known not to work should not
// stay in circulation.
func (m *Manager) swap(ctx context.Context, addr store.AddressStatus, provisionID int64, ov Override) error {
	configs := m.configCount(addr)

	created, address, choice, err := m.buildSomewhereReachable(ctx, addr, provisionID)
	if err != nil {
		// Nothing could be built. Abandoning here leaves users on a dead
		// address until somebody frees a slot by hand — which on a full account
		// can be days.
		//
		// The machine itself is fine: its disk, its certs and its configs all
		// work, and only its address is filtered. Giving it a new address needs
		// no slot at all, so it is tried before giving up.
		if rescue, rerr := m.rescueWithFloatingIP(ctx, addr, provisionID, ov); rerr == nil {
			return m.finishFloatingSwap(ctx, addr, provisionID, rescue, configs)
		} else if !errors.Is(rerr, ErrNoFloatingRescue) {
			m.log.Warn("the floating rescue failed too", "address", addr.Address, "err", rerr)
			err = fmt.Errorf("%w; the floating rescue also failed: %v", err, rerr)
		} else {
			m.log.Info("no floating rescue available", "address", addr.Address, "why", rerr)
			m.step(StepFloatSkip, rerr.Error())
		}
		return m.abandon(ctx, addr, provisionID, configs, err)
	}
	// No port is sent. Every config on this machine moves, each keeping the
	// port it already had — the replacement is a clone and listens on the same
	// ones. Passing the triggering port here is what moved 200 configs off 8443
	// and onto 443 without anyone asking.
	m.step(StepMoving, configs, addr.Address, address)
	result, err := m.zex.ReplaceAddress(ctx, zex.ReplaceRequest{
		OldAddress: addr.Address,
		NewAddress: address,
		Country:    choice.Country,
		Activate:   true,
	})
	if err != nil {
		return m.abandon(ctx, addr, provisionID, configs,
			fmt.Errorf("the new server is healthy but the panel swap failed: %w", err))
	}
	m.step(StepMoved, result.Updated, result.Skipped)
	m.progressNewAddress(address)

	// Only now is the old machine handed back. It has to be after the configs
	// have actually moved: until then they still point at it, and destroying it
	// first opens a window where a user holds a config for a server that no
	// longer exists — and if the panel call fails, the machine is gone and the
	// configs are still aimed at it.
	//
	// Freeing a slot to build into is a different problem and is solved where
	// it arises, in buildAcross, which retires the machine only once every
	// project has reported itself full.
	m.retireOldServer(ctx, addr, provisionID)

	if err := m.st.SetProvisionStatus(provisionID, store.ProvSwapped, true, nil, time.Now()); err != nil {
		m.log.Warn("could not record the swap", "err", err)
	}
	m.send(ctx, KindSwapped, addr.Trigger().TargetID, provisionID,
		swappedMessage(addr.Address, address, choice.Country, created,
			&replaceOutcome{Updated: result.Updated, Skipped: result.Skipped, Errors: result.Errors}))

	m.log.Info("replacement complete",
		"was", addr.Address, "now", address, "country", choice.Country,
		"configs", result.Updated, "skipped", result.Skipped)
	return nil
}

// configCount is how many configs point at this machine, for the alerts.
//
// Read from what the last scan recorded rather than by asking the panel: the
// number is only ever used to tell a person how much is affected, and a panel
// call here would be one more thing that can fail in the middle of a
// replacement.
func (m *Manager) configCount(addr store.AddressStatus) int {
	n := 0
	for _, p := range addr.Ports {
		n += len(p.ConfigIDs)
	}
	return n
}

// quiet takes every config on the failing address out of circulation.
//
// Only reached when a replacement could not be produced. While one is on its
// way the configs stay up: they point at an address Iran cannot reach, so
// switching them off takes nothing away, and leaving them means the cutover is
// a single call rather than a hole the length of a build.
func (m *Manager) quiet(ctx context.Context, addr store.AddressStatus, provisionID int64) (int, error) {
	count, err := m.zex.BulkActive(ctx, addr.Address, false)
	if err != nil {
		// The caller owns the provision status — it is already recording a
		// failure — so this only reports what could not be done.
		m.send(ctx, KindQuietFailed, addr.Trigger().TargetID, provisionID,
			quietFailedMessage(addr.Address, err))
		return 0, err
	}

	m.send(ctx, KindQuieted, addr.Trigger().TargetID, provisionID,
		quietedMessage(addr.Address, count))

	m.log.Info("configs taken out of circulation", "address", addr.Address, "configs", count)
	return count, nil
}

// build creates the server, trying each of the country's locations in turn.
func (m *Manager) build(ctx context.Context, addr store.AddressStatus, provisionID int64, candidates []buildCandidate) (*createdInfo, string, error) {
	if err := m.st.SetProvisionStatus(provisionID, store.ProvCreating, false, nil, time.Now()); err != nil {
		m.log.Warn("could not record the creating state", "err", err)
	}

	var lastErr error
	var skippedProject string

	for _, cand := range candidates {
		cli, ok := m.hz.Client(cand.Project)
		if !ok {
			continue
		}
		// A project that just reported itself full has nothing to offer in any
		// of its remaining locations either.
		if cand.Project == skippedProject {
			continue
		}

		spec := hetzner.CreateSpec{
			Name:       m.set.Get(settings.HetznerNamePrefix) + time.Now().UTC().Format("20060102-150405"),
			ServerType: cand.ServerType,
			Image:      cand.SnapshotID,
			Location:   cand.Location,
			SSHKeys:    cand.SSHKeys,
			Labels: map[string]string{
				"managed-by": "botchecker",
				"replaces":   addr.Address,
				// The project is stamped on the server so an orphan can be
				// traced back from the Hetzner console.
				"botchecker-project": cand.Project,
			},
		}
		location := cand.Location

		// Hold the address before buying the machine that will carry it. Left
		// to Hetzner, the pool can hand back the very address being replaced,
		// and that is only discovered after a boot and several minutes of
		// probing. Reserving first turns it into one call and a refusal.
		//
		// If the reservation cannot be made at all the build still goes ahead
		// on the old terms: an unreserved address is worse than a reserved one,
		// but far better than not replacing a blocked machine. The check after
		// creation catches a recycled address either way.
		if reserved, datacenter, err := m.reserveCleanIP(ctx, cli, cand); err != nil {
			if errors.Is(err, ErrNoCleanAddress) {
				m.log.Warn("every address this location offered is already dead, moving on",
					"project", cand.Project, "location", location, "err", err)
				lastErr = err
				continue
			}
			m.log.Warn("could not reserve an address, letting Hetzner pick one",
				"project", cand.Project, "location", location, "err", err)
		} else {
			spec.PrimaryIPv4 = reserved.ID
			spec.Datacenter = datacenter
			m.step(StepReserved, reserved.Address)
			m.log.Info("reserved an address for the replacement",
				"address", reserved.Address, "datacenter", datacenter)
		}

		created, err := cli.CreateFromSnapshot(ctx, spec)
		if err != nil {
			switch {
			case hetzner.IsLimitReached(err):
				m.log.Warn("project is at its server limit, moving to the next project",
					"project", cand.Project, "err", err)
				skippedProject = cand.Project
			default:
				// A type can be unavailable in one location and fine in the
				// next, so the rest of this project is still worth trying.
				m.log.Warn("could not create here, trying the next candidate",
					"project", cand.Project, "location", location, "err", err)
			}
			m.step(StepCreateFailed, location, err.Error())
			lastErr = err
			continue
		}

		if err := m.st.SetProvisionServer(provisionID, created.ID, created.ActionID,
			cand.SnapshotID, created.IPv4, 0); err != nil {
			m.log.Warn("could not record the created server", "err", err)
		}
		if created.ActionID != 0 {
			if err := cli.WaitAction(ctx, created.ActionID, m.cfg.ProvisionActionTimeout); err != nil {
				return nil, "", fmt.Errorf("server did not come up: %w", err)
			}
		}

		resource, err := cli.Server(ctx, created.ID)
		if err != nil {
			return nil, "", fmt.Errorf("read new server: %w", err)
		}
		if resource.Address == "" {
			return nil, "", errors.New("the new server has no public IPv4")
		}
		if err := m.st.SetProvisionServer(provisionID, created.ID, created.ActionID,
			cand.SnapshotID, resource.Address, 0); err != nil {
			m.log.Warn("could not record the new address", "err", err)
		}

		// The last line of defence. Reserving an address should have made this
		// impossible, but the reservation is allowed to fail softly above, and
		// a dead address must never reach verification: that is the twelve
		// minutes, and the server slot, that this whole path exists to save.
		if why := m.dirtyAddress(resource.Address); why != "" {
			m.log.Warn("the new server was handed an address that is already dead, deleting it",
				"address", resource.Address, "server_id", created.ID, "why", why)
			m.discard(ctx, &createdInfo{ServerID: created.ID, Project: cand.Project},
				resource.Address, scanner.VerdictUnknown)
			lastErr = fmt.Errorf("%s was handed %s, which is %s", location, resource.Address, why)
			m.step(StepCreateFailed, location, lastErr.Error())
			continue
		}

		info := &createdInfo{
			ServerID: created.ID, Address: resource.Address,
			ServerType: resource.ServerType, Location: resource.Location,
			// Carried, not looked up again later: the server may not be in any
			// inventory yet, and re-resolving it by address could answer with
			// the wrong project.
			Project: cand.Project,
		}
		m.send(ctx, KindProvisioned, addr.Trigger().TargetID, provisionID, provisionedMessage(addr.Address, info))
		m.step(StepCreated, created.ID, resource.Address, resource.ServerType)

		if err := m.st.SetProvisionStatus(provisionID, store.ProvBooting, false, nil, time.Now()); err != nil {
			m.log.Warn("could not record the booting state", "err", err)
		}
		if err := m.waitForBoot(ctx, resource.Address, addr.PortNumbers()); err != nil {
			return nil, "", err
		}
		return info, resource.Address, nil
	}

	if lastErr == nil {
		lastErr = errors.New("no location available")
	}
	return nil, "", fmt.Errorf("could not create a server in any of %d candidate locations: %w",
		len(candidates), lastErr)
}

// verifyAddress probes the new address from Iran with the same engine and the
// same verdict logic as the main scan — on every port the machine serves.
//
// Checking only the port that triggered the replacement would accept a server
// that answers on 443 and refuses on 8443, and the configs for 8443 would be
// moved onto it regardless. That is not a filtering problem, it is a broken
// clone, and the only way to tell is to ask each port.
//
// Ports are probed one at a time because the check-host client serialises
// every request behind one limiter anyway; goroutines would only queue. A round
// stops at the first port that fails, since the machine is already not usable
// and the remaining probes would buy nothing.
func (m *Manager) verifyAddress(ctx context.Context, address string, ports []int, provisionID int64) (scanner.Verdict, map[int]scanner.Verdict, error) {
	if err := m.st.SetProvisionStatus(provisionID, store.ProvVerifying, false, nil, time.Now()); err != nil {
		m.log.Warn("could not record the verifying state", "err", err)
	}
	if len(ports) == 0 {
		return scanner.VerdictUnknown, nil, errors.New("no ports to verify")
	}

	byPort := map[int]scanner.Verdict{}
	worst := scanner.VerdictUnknown
	m.step(StepTesting, address, joinInts(ports))

	for round := 1; round <= m.cfg.ProvisionVerifyRounds; round++ {
		byPort = map[int]scanner.Verdict{}
		allHealthy := true

		for _, port := range ports {
			hostPort := fmt.Sprintf("%s:%d", address, port)
			check, err := m.ch.CheckTCP(ctx, hostPort, m.cfg.AllNodes())
			if err != nil {
				m.log.Warn("verification probe failed",
					"address", hostPort, "round", round, "err", err)
				byPort[port] = scanner.VerdictUnknown
				allHealthy = false
				break
			}

			assess := scanner.Classify(check.Results, m.irNodes, m.cfg.MinIRFail, m.cfg.ControlEnabled())
			byPort[port] = assess.Verdict
			if assess.Verdict != scanner.VerdictHealthy {
				allHealthy = false
				break
			}
		}

		if allHealthy {
			worst = scanner.VerdictHealthy
			break
		}
		worst = worstVerdict(byPort)

		if round < m.cfg.ProvisionVerifyRounds {
			if err := sleep(ctx, m.cfg.ProvisionVerifyDelay); err != nil {
				return worst, byPort, err
			}
		}
	}

	// Only the aggregate is stored: the column is read as a verdict token, so a
	// per-port summary packed into it would render as nonsense.
	if err := m.st.SetProvisionVerdict(provisionID, string(worst), time.Now()); err != nil {
		m.log.Warn("could not record the verification verdict", "err", err)
	}
	return worst, byPort, nil
}

// worstVerdict picks the reading an operator most needs to see.
func worstVerdict(byPort map[int]scanner.Verdict) scanner.Verdict {
	rank := map[scanner.Verdict]int{
		scanner.VerdictHealthy:      0,
		scanner.VerdictPortClosed:   1,
		scanner.VerdictPartialBlock: 2,
		scanner.VerdictServerDown:   3,
		scanner.VerdictBlockedIR:    4,
		scanner.VerdictUnknown:      5,
	}
	worst := scanner.VerdictUnknown
	best := -1
	for _, v := range byPort {
		if r, ok := rank[v]; ok && r > best {
			best, worst = r, v
		}
	}
	return worst
}

// describePorts renders a per-port verification result for the alert.
func describePorts(byPort map[int]scanner.Verdict) string {
	ports := make([]int, 0, len(byPort))
	for p := range byPort {
		ports = append(ports, p)
	}
	sort.Ints(ports)

	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, fmt.Sprintf("%d=%s", p, byPort[p]))
	}
	return strings.Join(parts, " ")
}

// abandon records a failure and leaves the configs deactivated on purpose.
func (m *Manager) abandon(ctx context.Context, addr store.AddressStatus, provisionID int64, configs int, cause error) error {
	// This is where the configs come out of circulation, and the only place
	// they do. There is no replacement coming, so a config pointing at an
	// address no Iranian network can reach is worth nothing to the person
	// holding it, and leaving it there hides the failure behind a client that
	// simply never connects.
	if n, qerr := m.quiet(ctx, addr, provisionID); qerr != nil {
		m.log.Error("could not switch the configs off after abandoning",
			"address", addr.Address, "err", qerr)
	} else {
		configs = n
	}
	m.step(StepConfigsOff, configs)

	m.log.Error("replacement abandoned",
		"target", addr.Address, "configs_left_off", configs, "err", cause)

	if err := m.st.SetProvisionStatus(provisionID, store.ProvFailed, true, cause, time.Now()); err != nil {
		m.log.Warn("could not record the failure", "err", err)
	}
	m.send(ctx, KindSwapFailed, addr.Trigger().TargetID, provisionID,
		swapFailedMessage(addr.Address, configs, cause))
	return cause
}

func (m *Manager) quietWindow() time.Duration {
	return m.set.Duration(settings.ProvisionQuietWindow, m.cfg.ProvisionQuietWindow)
}

// buildSomewhereReachable buys servers until one of them answers from Iran.
//
// The old rule picked a country where every known address was healthy, and then
// trusted it. Measurement says that is the wrong question: on 2026-09-21 every
// address in Ashburn was blocked, yet a freshly created Ashburn address answered
// from all eight Iranian networks on the first try — while fresh addresses in
// Helsinki, Nuremberg and Singapore did not. What is filtered is the address,
// not the region, so the only thing worth trusting is a probe of the address
// itself.
//
// So locations are tried in the operator's order and the verification is the
// gate. A server whose address comes back filtered is deleted immediately:
// keeping it would spend money on something known not to work and burn one of
// the project's limited server slots.
func (m *Manager) buildSomewhereReachable(ctx context.Context, addr store.AddressStatus, provisionID int64) (*createdInfo, string, CountryHealth, error) {
	candidates, skipped := m.buildCandidates()
	if len(candidates) == 0 {
		reason := "no Hetzner project is configured to build in"
		if len(skipped) > 0 {
			reason += ": " + strings.Join(skipped, ", ")
		}
		return nil, "", CountryHealth{}, errors.New(reason)
	}
	candidates, noImage := m.withSnapshots(ctx, candidates)
	if len(candidates) == 0 {
		return nil, "", CountryHealth{}, errors.New("no project has a snapshot to build from: " +
			strings.Join(noImage, ", "))
	}
	return m.buildAcross(ctx, addr, provisionID, candidates, false)
}

// buildAcross walks the candidates until one produces an address Iran can
// reach. retired guards the single retry described at the bottom of the loop.
func (m *Manager) buildAcross(ctx context.Context, addr store.AddressStatus, provisionID int64,
	candidates []buildCandidate, retired bool) (*createdInfo, string, CountryHealth, error) {

	var lastErr error
	for _, cand := range candidates {
		location := cand.Location
		m.step(StepTryLocation, orDash(location), orDash(countryForLocation(location)), cand.Project, cand.SnapshotID)
		created, address, err := m.build(ctx, addr, provisionID, []buildCandidate{cand})
		if err != nil {
			lastErr = err
			m.log.Warn("could not build here, trying the next candidate",
				"project", cand.Project, "location", location, "err", err)
			continue
		}

		verdict, byPort, err := m.verifyAddress(ctx, address, addr.PortNumbers(), provisionID)
		if err != nil {
			// The probe itself failed, which says nothing about the address.
			// The server is kept and reported rather than silently destroyed.
			return nil, "", CountryHealth{}, err
		}

		healthy := verdict == scanner.VerdictHealthy
		m.send(ctx, KindVerified, addr.Trigger().TargetID, provisionID,
			verifiedMessage(addr.Address, address, describePorts(byPort), string(verdict), healthy))
		if healthy {
			m.step(StepHealthy, address, describePorts(byPort))
			m.log.Info("the new address answers from Iran",
				"address", address, "location", location)
			return created, address, CountryHealth{
				Country:   countryForLocation(location),
				Locations: []string{location},
			}, nil
		}

		m.log.Warn("the new address is not usable, discarding it",
			"address", address, "location", location, "verdict", verdict,
			"per_port", describePorts(byPort))
		// Recorded before the delete, because the delete is what puts this
		// address back in the pool for the next build to be handed.
		m.burn(address, store.BurnDiscarded, store.LedgerEntry{
			Verdict:     string(verdict),
			Project:     cand.Project,
			ServerID:    created.ServerID,
			ProvisionID: provisionID,
			Note:        "built in " + location + " and came back " + describePorts(byPort),
		})
		m.step(StepUnhealthy, address, string(verdict), describePorts(byPort))
		m.discard(ctx, created, address, verdict)
		m.step(StepDiscarded, address)
		lastErr = fmt.Errorf("%s in %s came back %s from Iran (%s)",
			address, location, verdict, describePorts(byPort))
	}

	if lastErr == nil {
		lastErr = errors.New("no location produced a usable address")
	}

	// Every project is full, and the one machine certainly not worth its slot
	// is the blocked one being replaced. It is handed back and the build tried
	// once more.
	//
	// Only after the first pass failed: a machine is never destroyed while
	// there was still room to build beside it, so the destructive step happens
	// only when it is the thing standing in the way.
	if hetzner.IsLimitReached(lastErr) && !retired {
		if m.retireOldServer(ctx, addr, provisionID) {
			m.log.Info("freed the blocked machine's slot, building again", "address", addr.Address)
			return m.buildAcross(ctx, addr, provisionID, candidates, true)
		}
	}
	return nil, "", CountryHealth{}, lastErr
}

// discard deletes a server whose address turned out to be filtered. A failure
// to delete is reported rather than retried: the address is named so it can be
// removed by hand, and the replacement attempt carries on regardless.
func (m *Manager) discard(ctx context.Context, created *createdInfo, address string, verdict scanner.Verdict) {
	if created == nil || created.ServerID == 0 {
		return
	}
	// The client of the project that created it. Server ids are per project, so
	// sending this id to any other account is either a no-op or a stranger's
	// machine.
	cli, ok := m.hz.Client(created.Project)
	if !ok {
		m.log.Error("cannot delete the filtered server: its project is gone from the configuration",
			"server_id", created.ServerID, "project", created.Project, "address", address)
		return
	}
	if err := cli.DeleteServer(context.WithoutCancel(ctx), created.ServerID); err != nil {
		m.log.Error("could not delete the filtered server — delete it by hand",
			"server_id", created.ServerID, "address", address, "err", err)
		return
	}
	m.log.Info("deleted the filtered server", "server_id", created.ServerID, "address", address)
}

// buildLocations is the operator's ordered preference, falling back to every
// location the country map mentions so an unset value still does something.
// buildCandidate is one place a replacement could be created: a project, a
// location inside it, and the resources that only exist behind that project's
// token.
type buildCandidate struct {
	Project    string
	Location   string
	ServerType string
	SnapshotID string
	SSHKeys    []string
}

// buildCandidates lists everywhere a replacement could be built, in the order
// they should be tried, and why anything was left out.
//
// Projects are walked in configured order and locations within each, because a
// snapshot only exists inside its own project and a server type is not offered
// in every location. A project that cannot build — no snapshot, no locations —
// is skipped with a reason rather than silently, since "nothing was built" is
// otherwise impossible to explain.
func (m *Manager) buildCandidates() ([]buildCandidate, []string) {
	var out []buildCandidate
	var skipped []string

	for _, p := range m.hz.Projects() {
		if ok, why := p.Usable(); !ok {
			skipped = append(skipped, fmt.Sprintf("%s (%s)", p.Slug, why))
			continue
		}
		locations := p.Locations
		if len(locations) == 0 {
			// No preference: one candidate with no location, and Hetzner picks.
			locations = []string{""}
		}
		for _, loc := range locations {
			out = append(out, buildCandidate{
				Project:    p.Slug,
				Location:   loc,
				ServerType: p.ServerTypeFor(loc),
				SnapshotID: p.SnapshotID,
				SSHKeys:    p.SSHKeys,
			})
		}
	}
	return out, skipped
}

// countryForLocation names the country a Hetzner location sits in, for the
// config renaming. Unknown locations get no prefix rather than a wrong one.
func countryForLocation(location string) string {
	switch strings.ToLower(strings.TrimSpace(location)) {
	case "ash", "hil":
		return "US"
	case "fsn1", "nbg1":
		return "DE"
	case "hel1":
		return "FI"
	case "sin":
		return "SG"
	default:
		return ""
	}
}

// retireOldServer hands back the machine being replaced, and reports whether it
// actually went.
//
// This is only ever reached for a machine that answered from no Iranian network
// and whose configs are already switched off, so nothing is being taken from
// anyone. A blocked address does not recover — the filtering is on the address
// itself — so keeping the server means paying for something that serves nobody
// and holding a slot in a project with a hard limit, which is the thing that
// stops the next replacement being built at all.
//
// It refuses rather than guesses in three cases, because the cost of being
// wrong here is a stranger's machine:
//   - no project recorded, so there is no safe client to send the id to
//   - the address is not a server in that project (a floating or primary IP,
//     or something already gone)
//   - the project cannot be read, so the id cannot be confirmed
func (m *Manager) retireOldServer(ctx context.Context, addr store.AddressStatus, provisionID int64) bool {
	project, err := m.st.AddressProject(addr.Address)
	if err != nil || project == "" {
		m.log.Info("keeping the old machine: no project is recorded for it",
			"address", addr.Address)
		return false
	}
	cli, ok := m.hz.Client(project)
	if !ok {
		m.log.Warn("keeping the old machine: its project is gone from the configuration",
			"address", addr.Address, "project", project)
		return false
	}

	inv, err := cli.Inventory(ctx)
	if err != nil {
		m.log.Warn("keeping the old machine: could not read its project", "err", err)
		return false
	}
	res, ok := inv.Lookup(addr.Address)
	if !ok || res.ServerID == 0 {
		m.log.Info("keeping the old machine: it is not a server in this project",
			"address", addr.Address, "kind", res.Kind)
		return false
	}

	if err := cli.DeleteServer(context.WithoutCancel(ctx), res.ServerID); err != nil {
		m.log.Error("could not hand back the blocked machine — do it by hand",
			"address", addr.Address, "server_id", res.ServerID, "project", project, "err", err)
		return false
	}
	m.hz.Invalidate(project)

	// The machine is gone, so Hetzner may offer this address to the next server
	// built in the same location. Without this record it would be bought back
	// and probed all over again.
	m.burn(addr.Address, store.BurnRetired, store.LedgerEntry{
		Verdict:     addr.Trigger().Verdict,
		Project:     project,
		ServerID:    res.ServerID,
		ProvisionID: provisionID,
		Note:        "handed back while replacing it",
	})

	m.log.Info("handed back the blocked machine",
		"address", addr.Address, "server_id", res.ServerID, "project", project)
	m.step(StepRetired, addr.Address, res.ServerID)
	m.send(ctx, KindRetired, addr.Trigger().TargetID, provisionID,
		retiredMessage(addr.Address, res.ServerName, res.ServerID, project))
	return true
}
