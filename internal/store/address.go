package store

import (
	"errors"
	"sort"
	"time"
)

// ErrNoSuchAddress is returned when nothing is known about a machine.
var ErrNoSuchAddress = errors.New("no such address")

// AddressStatus is every port of one machine, judged together.
//
// Filtering is applied to the address, not to the port: across the live fleet
// every dual-port address has read identically on both ports, and the panel
// operations that act on a blocked machine — deactivating its configs, moving
// them to a new address — already take an address and no port at all. Deciding
// per port while acting per address is what let one port's verdict drag its
// siblings along, so the decision is made here, once, for the whole machine.
//
// Evidence stays per port: results, node_results and events are untouched.
type AddressStatus struct {
	Address     string         `json:"address"`
	CountryCode string         `json:"country_code"`
	Ports       []TargetStatus `json:"ports"`
}

// Addresses groups per-port statuses into one entry per machine, in the order
// TargetStatuses returned them.
func Addresses(statuses []TargetStatus) []AddressStatus {
	byAddress := map[string]*AddressStatus{}
	var order []string

	for _, s := range statuses {
		a, ok := byAddress[s.Address]
		if !ok {
			a = &AddressStatus{Address: s.Address, CountryCode: s.CountryCode}
			byAddress[s.Address] = a
			order = append(order, s.Address)
		}
		// The first non-empty country wins; a manually added port carries none.
		if a.CountryCode == "" {
			a.CountryCode = s.CountryCode
		}
		a.Ports = append(a.Ports, s)
	}

	out := make([]AddressStatus, 0, len(order))
	for _, addr := range order {
		a := byAddress[addr]
		sort.Slice(a.Ports, func(i, j int) bool { return a.Ports[i].Port < a.Ports[j].Port })
		out = append(out, *a)
	}
	return out
}

// AddressStatuses loads every machine with all of its ports.
func (s *Store) AddressStatuses() ([]AddressStatus, error) {
	statuses, err := s.TargetStatuses()
	if err != nil {
		return nil, err
	}
	return Addresses(statuses), nil
}

// AddressFor returns one machine and all of its ports.
func (s *Store) AddressFor(address string) (AddressStatus, error) {
	all, err := s.AddressStatuses()
	if err != nil {
		return AddressStatus{}, err
	}
	for _, a := range all {
		if a.Address == address {
			return a, nil
		}
	}
	return AddressStatus{}, ErrNoSuchAddress
}

// TargetIDs lists every port's target id, which is what the per-port tables are
// keyed by.
func (a AddressStatus) TargetIDs() []int64 {
	out := make([]int64, 0, len(a.Ports))
	for _, p := range a.Ports {
		out = append(out, p.TargetID)
	}
	return out
}

// PortNumbers lists the ports this machine serves.
func (a AddressStatus) PortNumbers() []int {
	out := make([]int, 0, len(a.Ports))
	for _, p := range a.Ports {
		out = append(out, p.Port)
	}
	return out
}

// FullyBlocked reports whether Iran cannot reach this machine on any port.
//
// This is the only condition the operator wants replaced automatically: a
// machine still reachable on one network is carrying users there, and taking it
// away to fix something half broken would cut them off.
func (a AddressStatus) FullyBlocked() bool {
	if len(a.Ports) == 0 {
		return false
	}
	for _, p := range a.Ports {
		if p.IRTotal == 0 || p.IROpen > 0 {
			return false
		}
	}
	return true
}

// Reachable is how many Iranian probes still get through on the best port.
func (a AddressStatus) Reachable() (open, total int) {
	for _, p := range a.Ports {
		if p.IROpen > open {
			open = p.IROpen
		}
		if p.IRTotal > total {
			total = p.IRTotal
		}
	}
	return open, total
}

// NotifyOnly is true when any port is marked hands-off. The flag is per port in
// the schema but the action it suppresses is address-wide, so one port asking to
// be left alone protects the whole machine.
func (a AddressStatus) NotifyOnly() bool {
	for _, p := range a.Ports {
		if p.NotifyOnly {
			return true
		}
	}
	return false
}

// Trigger is the port whose evidence is reported and recorded. It is the worst
// one, so an alert quotes the most broken reading rather than an arbitrary port.
func (a AddressStatus) Trigger() TargetStatus {
	if len(a.Ports) == 0 {
		return TargetStatus{}
	}
	worst := a.Ports[0]
	for _, p := range a.Ports[1:] {
		if p.IROpen < worst.IROpen {
			worst = p
		}
	}
	return worst
}

// CheckedAt is the most recent probe across the machine's ports.
func (a AddressStatus) CheckedAt() *time.Time {
	var newest *time.Time
	for _, p := range a.Ports {
		if p.CheckedAt == nil {
			continue
		}
		if newest == nil || p.CheckedAt.After(*newest) {
			newest = p.CheckedAt
		}
	}
	return newest
}

// ---- orphan reporting ----

// OrphanReportedAt is when a server with no config behind it was last reported,
// or nil if it never was.
//
// Kept in the settings table rather than a column on targets: an orphan is by
// definition an address with no target row, so there is nowhere on targets to
// record it.
func (s *Store) OrphanReportedAt(address string) (*time.Time, error) {
	var at string
	err := s.db.QueryRow(
		`SELECT value FROM settings WHERE key = ?`, orphanKey(address)).Scan(&at)
	if err != nil {
		// Missing is the common case and is not an error worth surfacing.
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, nil
	}
	return &t, nil
}

// MarkOrphanReported records that the operator has been told about this server,
// so the same alert is not repeated on every scan.
func (s *Store) MarkOrphanReported(address string, at time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO settings (key, value, secret, updated_at) VALUES (?, ?, 0, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		orphanKey(address), at.UTC().Format(time.RFC3339Nano), at.UTC().Format(time.RFC3339Nano))
	return err
}

func orphanKey(address string) string { return "orphan.reported." + address }

// BurnedReportedAt and MarkBurnedReported keep the "this machine is retired and
// its configs still point at it" alert to once a day per address.
//
// Stored the same way as the orphan marker, and for the same reason: it is a
// note about an address rather than about a row, and the address may outlive
// every target row it ever had.
func (s *Store) BurnedReportedAt(address string) (*time.Time, error) {
	return s.reportedAt(burnedKey(address))
}

func (s *Store) MarkBurnedReported(address string, at time.Time) error {
	return s.markReported(burnedKey(address), at)
}

func burnedKey(address string) string { return "burned.reported." + address }

func (s *Store) reportedAt(key string) (*time.Time, error) {
	var at string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&at); err != nil {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, nil
	}
	return &t, nil
}

func (s *Store) markReported(key string, at time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO settings (key, value, secret, updated_at) VALUES (?, ?, 0, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, ts(at), ts(at))
	return err
}
