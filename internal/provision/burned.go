package provision

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/vefgh/botchecker/internal/store"
)

// ReportBurned finds machines that are dead on purpose but still in the app.
//
// An address enters the ledger when this service destroys it: retired during a
// replacement, discarded for coming back filtered, or deleted from the bot.
// Nothing removes its configs, so the panel keeps handing it out, the scan
// keeps probing it, and it keeps failing. Acting on that would buy a machine to
// replace one that no longer exists — which is the loop this ledger ends.
//
// So it is reported and never acted on, once a day per address. Silence would
// be worse than the loop: users would be given an address that answers nothing
// and nobody would know. Only the operator can decide where those configs
// should point instead.
func (m *Manager) ReportBurned(ctx context.Context) error {
	known, err := m.st.AddressStatuses()
	if err != nil {
		return err
	}
	burned, err := m.st.ActiveBurnedAddresses()
	if err != nil {
		return err
	}
	if len(burned) == 0 {
		return nil
	}

	var due []store.AddressStatus
	for _, a := range known {
		if _, ok := burned[a.Address]; !ok {
			continue
		}
		seen, err := m.st.BurnedReportedAt(a.Address)
		if err != nil {
			return err
		}
		if seen != nil && time.Since(*seen) < m.orphanInterval() {
			continue
		}
		due = append(due, a)
	}
	if len(due) == 0 {
		return nil
	}
	sort.Slice(due, func(i, j int) bool { return due[i].Address < due[j].Address })

	now := time.Now()
	for _, a := range due {
		entry := burned[a.Address]
		configs := 0
		for _, p := range a.Ports {
			configs += len(p.ConfigIDs)
		}
		m.log.Info("a retired machine is still in the app",
			"address", a.Address, "reason", entry.Reason, "configs", configs)
		m.send(ctx, KindBurned, a.Trigger().TargetID, 0,
			burnedMessage(a.Address, entry, configs, a.PortNumbers()))
		if err := m.st.MarkBurnedReported(a.Address, now); err != nil {
			return err
		}
	}
	return nil
}

// refuseBurned stops a replacement for an address that is in the ledger.
//
// The periodic report above is what tells the operator; this is the guard that
// stops the machinery. It exists separately because a replacement can also be
// started by hand from the bot, and a button must not be able to do what the
// automatic path is forbidden to do.
func (m *Manager) refuseBurned(addr store.AddressStatus, entry store.LedgerEntry,
	outageCount int, now time.Time) error {

	target := addr.Trigger()
	reason := fmt.Errorf("skipped: %s is in the ledger (%s, recorded %s) — release it first",
		addr.Address, entry.Reason, entry.RecordedAt.UTC().Format("2006-01-02 15:04"))

	m.log.Info("machine is in the ledger, not replacing it",
		"address", addr.Address, "reason", entry.Reason, "recorded", entry.RecordedAt)

	id, err := m.st.CreateProvision(target.TargetID, "", m.dryRun(), outageCount, nil, now)
	if err != nil {
		return err
	}
	return m.st.SetProvisionStatus(id, store.ProvBurned, true, reason, time.Now())
}
