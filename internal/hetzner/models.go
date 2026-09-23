// Package hetzner talks to the Hetzner Cloud API: it tells which of the
// monitored addresses actually belong to the configured project, and creates
// replacement servers from a snapshot.
package hetzner

import (
	"sort"
	"time"
)

// Ownership says whether an address can be replaced automatically.
type Ownership string

const (
	// OwnedHetzner means the address was found in the configured project.
	OwnedHetzner Ownership = "hetzner"
	// OwnedExternal means the token is valid but the address is not in this
	// project — it belongs to another provider or another Hetzner project,
	// and must be replaced by hand.
	OwnedExternal Ownership = "external"
	// OwnedUnknown means no token is configured, so nothing can be said.
	// It must never be reported as external.
	OwnedUnknown Ownership = "unknown"
)

// Resource is one address found in the project.
type Resource struct {
	Address    string `json:"address"`
	Kind       string `json:"kind"` // server | primary_ip | floating_ip
	ServerID   int64  `json:"server_id,omitempty"`
	ServerName string `json:"server_name,omitempty"`
	ServerType string `json:"server_type,omitempty"`
	Location   string `json:"location,omitempty"`
	Status     string `json:"status,omitempty"`
	// AbuseBlocked is Hetzner's own anti-abuse block, which is a different
	// problem from Iranian filtering and is not fixed by a new server.
	AbuseBlocked bool `json:"abuse_blocked"`
}

// Inventory is a snapshot of every address in the project.
type Inventory struct {
	byAddress map[string]Resource
	FetchedAt time.Time
	// Servers is the number of servers seen, for the settings test button.
	Servers int
}

// Lookup resolves one address.
func (i *Inventory) Lookup(address string) (Resource, bool) {
	if i == nil {
		return Resource{}, false
	}
	r, ok := i.byAddress[address]
	return r, ok
}

// Ownership classifies one address against this inventory.
func (i *Inventory) Ownership(address string) Ownership {
	if i == nil {
		return OwnedUnknown
	}
	if _, ok := i.byAddress[address]; ok {
		return OwnedHetzner
	}
	return OwnedExternal
}

// Len reports how many addresses the project holds.
// Resources lists everything the project holds, in address order. Used by the
// checks that ask "what is in this project that nothing else knows about".
func (i *Inventory) Resources() []Resource {
	if i == nil {
		return nil
	}
	out := make([]Resource, 0, len(i.byAddress))
	for _, r := range i.byAddress {
		out = append(out, r)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Address < out[b].Address })
	return out
}

func (i *Inventory) Len() int {
	if i == nil {
		return 0
	}
	return len(i.byAddress)
}

// Snapshot is a stored image new servers are created from.
type Snapshot struct {
	ID          int64             `json:"id"`
	Description string            `json:"description"`
	Created     time.Time         `json:"created"`
	ImageSize   float64           `json:"image_size"`
	Labels      map[string]string `json:"labels"`
	Status      string            `json:"status"`
}

// CreateSpec describes the server to create.
type CreateSpec struct {
	Name       string
	ServerType string
	Image      string
	Location   string
	SSHKeys    []string
	Labels     map[string]string
	// Datacenter pins the site. It is set only alongside PrimaryIPv4, because a
	// reserved address belongs to one datacenter and the server carrying it must
	// be created there. Setting it replaces Location, which Hetzner rejects if
	// both are sent.
	Datacenter string
	// PrimaryIPv4 is an address reserved before this call. Zero means Hetzner
	// picks one from the pool, which is how a recycled address gets in.
	PrimaryIPv4 int64
}

// CreatedServer is the result of a successful creation.
type CreatedServer struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	IPv4     string `json:"ipv4"`
	ActionID int64  `json:"action_id"`
	Status   string `json:"status"`
}

// PrimaryIP is an address reserved ahead of the server that will carry it.
//
// Reserving first is what makes the address knowable before anything is bought:
// a server's own IP is only visible once it exists, and by then a recycled
// address has already cost a slot and a boot.
type PrimaryIP struct {
	ID         int64  `json:"id"`
	Address    string `json:"address"`
	Datacenter string `json:"datacenter,omitempty"`
	AssigneeID int64  `json:"assignee_id,omitempty"`
}

// FloatingIP is an address that can be moved between servers. Unlike a primary
// IP it needs configuring inside the server's operating system before packets
// sent to it are answered.
type FloatingIP struct {
	ID           int64  `json:"id"`
	Address      string `json:"address"`
	HomeLocation string `json:"home_location,omitempty"`
	ServerID     int64  `json:"server_id,omitempty"`
}

// Datacenter is one site inside a location. Primary IPs are reserved per
// datacenter, not per location, so a server that is to carry a reserved address
// must be created in the same datacenter.
type Datacenter struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Location string `json:"location"`
	// AvailableTypes is the set of server types that can actually be created
	// here right now. Checking it turns "server type not available" from a
	// failed create into a candidate that is never tried.
	AvailableTypes map[int64]bool `json:"-"`
}
