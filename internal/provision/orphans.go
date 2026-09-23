package provision

import (
	"context"
	"sort"
	"time"
)

// ReportOrphans finds servers the app never hands out, and says so once.
//
// A server that no config points at is invisible to every other part of this
// service: the config list is where addresses come from, so a machine missing
// from it is never probed, never alerted on, and never replaced — while still
// being billed and still holding a slot in a project with a hard server limit.
//
// It is only ever reported, never acted on. A machine can be absent from the
// config list because it is mid-setup, because it serves something this service
// does not know about, or because it was genuinely forgotten, and nothing here
// can tell those apart. Only the operator can.
func (m *Manager) ReportOrphans(ctx context.Context) error {
	if m.hz == nil || !m.hz.Configured() {
		return nil
	}

	known, err := m.st.AddressStatuses()
	if err != nil {
		return err
	}
	inConfig := make(map[string]bool, len(known))
	for _, a := range known {
		inConfig[a.Address] = true
	}

	invs, errs := m.hz.InventoryAll(ctx)
	if len(errs) > 0 {
		// A project that could not be read would have every one of its servers
		// reported as an orphan. Saying nothing is the only honest option.
		for slug, err := range errs {
			m.log.Warn("skipping the orphan check: a project could not be read",
				"project", slug, "err", err)
		}
		return nil
	}

	var found []orphanRow
	for slug, inv := range invs {
		for _, res := range inv.Resources() {
			if res.ServerID == 0 || inConfig[res.Address] {
				continue
			}
			found = append(found, orphanRow{
				Address: res.Address, ServerName: res.ServerName,
				ServerID: res.ServerID, Project: slug,
			})
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Address < found[j].Address })

	// Reported once per address. Repeating it every scan would train the reader
	// to ignore the alert, and the whole value of it is that it gets read.
	var fresh []orphanRow
	for _, r := range found {
		seen, err := m.st.OrphanReportedAt(r.Address)
		if err != nil {
			return err
		}
		if seen != nil && time.Since(*seen) < m.orphanInterval() {
			continue
		}
		fresh = append(fresh, r)
	}
	if len(fresh) == 0 {
		return nil
	}

	m.log.Info("servers found that no config points at", "count", len(fresh))
	m.send(ctx, KindOrphan, 0, 0, orphanMessage(fresh))

	now := time.Now()
	for _, r := range fresh {
		if err := m.st.MarkOrphanReported(r.Address, now); err != nil {
			return err
		}
	}
	return nil
}

// orphanInterval is how long an already-reported orphan stays quiet.
func (m *Manager) orphanInterval() time.Duration { return 24 * time.Hour }
