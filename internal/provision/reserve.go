package provision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/scanner"
	"github.com/vefgh/botchecker/internal/settings"
	"github.com/vefgh/botchecker/internal/store"
)

// This is the answer to a machine being bought twice. On 2026-09-21 three
// replacements were handed an address this service had itself thrown away
// earlier the same day, one of them twice five hours apart: Hetzner returns a
// deleted server's IP to its location's pool, and nothing here remembered what
// had been deleted. Each round cost a server slot out of five and up to twelve
// minutes of probing before the address was recognised and discarded again.
//
// Reserving the address first turns that into one API call and a rejection.

// defaultIPAttempts is how many addresses are looked at in one location before
// moving on. The pool can return a burned address more than once, but a
// location that cannot produce a clean one in a handful of tries is better
// abandoned for the next location than hammered.
const defaultIPAttempts = 5

// ErrNoCleanAddress means every address the pool offered here is already known
// to be dead.
var ErrNoCleanAddress = errors.New("no clean address available in this location")

// dirtyAddress explains why an address must not be used, or "" when it is fine.
//
// Two sources are consulted and they say different things. The ledger is what
// this service destroyed or retired on purpose. The targets table is what it
// has measured — an address still being probed with a broken reading is one the
// configs are already pointing at, so handing it out as a replacement would
// move users from one dead address to the same dead address.
func (m *Manager) dirtyAddress(address string) string {
	if address == "" {
		return ""
	}
	if burned, entry, err := m.st.IsBurned(address); err != nil {
		m.log.Warn("could not read the address ledger", "address", address, "err", err)
	} else if burned {
		when := entry.RecordedAt.UTC().Format("2006-01-02 15:04")
		if entry.Verdict != "" {
			return fmt.Sprintf("in the ledger since %s (%s, read %s)", when, entry.Reason, entry.Verdict)
		}
		return fmt.Sprintf("in the ledger since %s (%s)", when, entry.Reason)
	}

	known, err := m.st.AddressFor(address)
	if err != nil {
		// Not a monitored address, which is the normal case for something
		// brand new.
		return ""
	}
	for _, p := range known.Ports {
		if p.Verdict != "" && p.Verdict != string(scanner.VerdictHealthy) {
			return fmt.Sprintf("already monitored on port %d and reading %s", p.Port, p.Verdict)
		}
	}
	return ""
}

// reserveCleanIP picks a datacenter that can build this candidate and holds an
// address there that is not already known to be dead.
//
// The reservation is made with auto_delete so it goes when the server does. A
// rejected one is deleted explicitly: Hetzner bills every primary IP that
// finished creating, assigned or not.
func (m *Manager) reserveCleanIP(ctx context.Context, cli *hetzner.Client, cand buildCandidate) (*hetzner.PrimaryIP, string, error) {
	datacenter, err := cli.DatacenterFor(ctx, cand.Location, cand.ServerType)
	if err != nil {
		return nil, "", err
	}

	attempts := m.set.Int(settings.ProvisionIPAttempts, defaultIPAttempts)
	if attempts < 1 {
		attempts = 1
	}

	var rejected []string
	for i := 1; i <= attempts; i++ {
		name := fmt.Sprintf("%sip-%s", m.set.Get(settings.HetznerNamePrefix), time.Now().UTC().Format("20060102-150405.000"))
		ip, err := cli.CreatePrimaryIP(ctx, name, datacenter, true)
		if err != nil {
			return nil, "", fmt.Errorf("reserve an address in %s: %w", datacenter, err)
		}

		why := m.dirtyAddress(ip.Address)
		if why == "" {
			if len(rejected) > 0 {
				m.log.Info("reserved a clean address after refusing recycled ones",
					"address", ip.Address, "datacenter", datacenter, "refused", rejected)
			}
			return ip, datacenter, nil
		}

		m.log.Info("the pool offered an address that is already dead, asking for another",
			"address", ip.Address, "datacenter", datacenter, "why", why, "attempt", i)
		rejected = append(rejected, ip.Address)
		// Deleted rather than kept: an unassigned primary IP is billed, and
		// holding it would not stop the pool offering it again anyway.
		if derr := cli.DeletePrimaryIP(context.WithoutCancel(ctx), ip.ID); derr != nil {
			m.log.Error("could not release a rejected address — delete it by hand",
				"address", ip.Address, "primary_ip_id", ip.ID, "err", derr)
		}
	}

	return nil, "", fmt.Errorf("%w: %s kept offering %s", ErrNoCleanAddress,
		datacenter, strings.Join(rejected, ", "))
}

// burn records an address as one that must never be handed out again.
// Failures are logged rather than returned: the caller is always in the middle
// of cleaning up after something worse, and losing the ledger write must not
// stop that.
func (m *Manager) burn(address, reason string, e store.LedgerEntry) {
	if address == "" {
		return
	}
	e.Address, e.Reason = address, reason
	if e.RecordedAt.IsZero() {
		e.RecordedAt = time.Now()
	}
	if err := m.st.BurnAddress(e); err != nil {
		m.log.Error("could not record the address in the ledger — it may be bought again",
			"address", address, "reason", reason, "err", err)
		return
	}
	m.log.Info("address recorded as dead", "address", address, "reason", reason)
}
