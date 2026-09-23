package provision

import (
	"context"
	"strings"

	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/settings"
	"github.com/vefgh/botchecker/internal/store"
)

// Plan is what pressing "replace now" would do, worked out before anything is
// touched: whether every service it depends on answers, and the exact changes
// in the order they would be made.
//
// It is read-only. It lists, pings and logs in, and never creates, assigns or
// deletes — so it can be shown on the confirmation screen and thrown away.
type Plan struct {
	Address string
	Checks  []PlanLine
	Steps   []PlanLine
	// Blocked means at least one check makes the run pointless or unsafe.
	Blocked bool
}

// PlanLine is one translated line: a key in internal/i18n and its arguments.
type PlanLine struct {
	Level string // PlanOK, PlanWarn, PlanBlock; empty for steps
	Key   string
	Args  []any
}

const (
	PlanOK    = "ok"
	PlanWarn  = "warn"
	PlanBlock = "block"
)

func (p *Plan) check(level, key string, args ...any) {
	p.Checks = append(p.Checks, PlanLine{Level: level, Key: key, Args: args})
	if level == PlanBlock {
		p.Blocked = true
	}
}

func (p *Plan) step(key string, args ...any) {
	p.Steps = append(p.Steps, PlanLine{Key: key, Args: args})
}

// Plan works out a replacement of addr without starting one.
func (m *Manager) Plan(ctx context.Context, addr store.AddressStatus, ov Override) Plan {
	p := Plan{Address: addr.Address}
	configs := m.configCount(addr)

	if m.Running() {
		p.check(PlanBlock, "plan.busy")
	}
	if burned, _, err := m.st.IsBurned(addr.Address); err == nil && burned {
		p.check(PlanBlock, "plan.burned")
	}
	if addr.NotifyOnly() {
		p.check(PlanBlock, "plan.notifyonly")
	}
	if m.dryRun() {
		p.check(PlanBlock, "plan.dryrun")
	}
	if reason := m.blockedFromProvisioning(ov); reason != "" {
		p.check(PlanBlock, "plan.guard", reason)
	}

	// The panel moves the configs. Without it the run can only build.
	switch {
	case m.zex == nil || !m.zex.Configured():
		p.check(PlanBlock, "plan.nopanel")
	default:
		if err := m.zex.Ping(ctx); err != nil {
			p.check(PlanBlock, "plan.panelerr", err.Error())
		} else {
			p.check(PlanOK, "plan.panelok")
		}
	}

	// check-host is the only judge of whether a new address works. If it is
	// unreachable every new server would be kept, unproven, and nothing moved.
	if _, err := m.ch.Nodes(ctx); err != nil {
		p.check(PlanBlock, "plan.checkhosterr", err.Error())
	} else {
		p.check(PlanOK, "plan.checkhostok", len(m.irNodes))
	}

	owner, resource, project := m.ownership(ctx, addr.Address)
	oldLocation := ""
	switch owner {
	case hetzner.OwnedHetzner:
		oldLocation = resource.Location
		p.check(PlanOK, "plan.owner", project, orDash(resource.ServerName), resource.ServerID, orDash(resource.Location))
	case hetzner.OwnedUnknown:
		p.check(PlanBlock, "plan.ownerunknown")
	default:
		p.check(PlanBlock, "plan.notmine")
	}

	// Every project that could build, with the image it would use and the room
	// it has. One lookup per project, however many locations it lists.
	candidates, skipped := m.buildCandidates()
	for _, s := range skipped {
		p.check(PlanWarn, "plan.projectskipped", s)
	}
	canBuild := map[string]bool{}
	var buildable []buildCandidate
	for _, c := range candidates {
		ok, seen := canBuild[c.Project]
		if !seen {
			ok = m.planProject(ctx, &p, c.Project, c.SnapshotID)
			canBuild[c.Project] = ok
		}
		if ok {
			buildable = append(buildable, c)
		}
	}
	if len(buildable) == 0 {
		p.check(PlanWarn, "plan.nobuild")
	}

	floatOn := m.set.Bool(settings.ProvisionFloatOnFailure) || ov.Manual
	floatReady := floatOn && m.sshConfigured() && owner == hetzner.OwnedHetzner && resource.ServerID != 0
	switch {
	case !floatOn:
		p.check(PlanWarn, "plan.floatoff")
	case !m.sshConfigured():
		p.check(PlanWarn, "plan.floatnossh")
	case floatReady:
		p.check(PlanOK, "plan.floatok", orDash(oldLocation))
	}
	if len(buildable) == 0 && !floatReady {
		p.check(PlanBlock, "plan.nothing")
	}

	// The changes, in the order they would happen.
	if len(buildable) > 0 {
		p.step("plan.step.build", describeCandidates(buildable))
		p.step("plan.step.test", joinInts(addr.PortNumbers()), len(m.irNodes))
	}
	if floatReady {
		p.step("plan.step.float", orDash(oldLocation))
	}
	p.step("plan.step.move", configs, addr.Address)
	if len(buildable) > 0 && owner == hetzner.OwnedHetzner && resource.ServerID != 0 {
		p.step("plan.step.retire", orDash(resource.ServerName), resource.ServerID, project)
	}
	p.step("plan.step.fail", configs)
	return p
}

// planProject checks one project can be reached, has room, and has an image,
// and reports whether a build there is worth attempting. A full project still
// is: the machine being replaced is handed back to make room.
func (m *Manager) planProject(ctx context.Context, p *Plan, slug, configured string) bool {
	cli, ok := m.hz.Client(slug)
	if !ok {
		p.check(PlanWarn, "plan.projecterr", slug, "not configured")
		return false
	}
	inv, err := cli.Inventory(ctx)
	if err != nil {
		p.check(PlanWarn, "plan.projecterr", slug, err.Error())
		return false
	}
	max := 0
	for _, pr := range m.hz.Projects() {
		if pr.Slug == slug {
			max = pr.MaxServers
		}
	}
	switch {
	case max > 0 && inv.Servers >= max:
		p.check(PlanWarn, "plan.projectfull", slug, inv.Servers, max)
	case max > 0:
		p.check(PlanOK, "plan.projectroom", slug, inv.Servers, max)
	default:
		p.check(PlanOK, "plan.projectok", slug, inv.Servers)
	}

	choice, err := m.resolveSnapshot(ctx, slug, configured)
	switch {
	case err != nil:
		p.check(PlanWarn, "plan.nosnapshot", slug, err.Error())
		return false
	case choice.Auto:
		p.check(PlanOK, "plan.snapshotauto", slug, choice.ID, orDash(choice.Description),
			orDash(choice.Created), orDash(choice.Configured))
	default:
		p.check(PlanOK, "plan.snapshot", slug, choice.ID, orDash(choice.Description), orDash(choice.Created))
	}
	return true
}

// describeCandidates renders the build order, e.g. "ash (US) → hel1 (FI)".
func describeCandidates(cands []buildCandidate) string {
	projects := map[string]bool{}
	for _, c := range cands {
		projects[c.Project] = true
	}
	parts := make([]string, 0, len(cands))
	for _, c := range cands {
		loc := c.Location
		if loc == "" {
			loc = "any"
		}
		part := loc
		if cc := countryForLocation(c.Location); cc != "" {
			part += " (" + cc + ")"
		}
		if len(projects) > 1 {
			part = c.Project + "/" + part
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " → ")
}
