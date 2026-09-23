package hetzner

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vefgh/botchecker/internal/settings"
)

// ErrAmbiguousOwner means two projects both claim an address.
//
// A public IPv4 cannot really be in two Hetzner projects at once, so this means
// a stale cache or a move in flight — and those are exactly the conditions
// under which guessing gets the wrong machine deleted.
var ErrAmbiguousOwner = errors.New("more than one project claims this address")

// Registry is every configured Hetzner project, each with its own client.
//
// Each project keeps a separate client and a separate inventory cache, because
// a token is only valid for its own project and an inventory only describes
// that project. Sharing either would let one project's answer stand in for
// another's, which is how a delete ends up in the wrong account.
type Registry struct {
	cfg     *settings.Provider
	log     *slog.Logger
	timeout time.Duration
	ttl     time.Duration
	base    string

	mu          sync.Mutex
	projects    []Project
	clients     map[string]*Client
	fingerprint string
	// skippedReport is the last misconfiguration reported, so the same one is
	// not logged on every sync.
	skippedReport string
}

func NewRegistry(cfg *settings.Provider, log *slog.Logger, timeout, inventoryTTL time.Duration) *Registry {
	return &Registry{cfg: cfg, log: log, timeout: timeout, ttl: inventoryTTL, clients: map[string]*Client{}}
}

// WithBaseURL points every client at a stub API, for tests.
func (r *Registry) WithBaseURL(u string) *Registry {
	r.mu.Lock()
	r.base = u
	r.clients = map[string]*Client{}
	r.fingerprint = ""
	r.mu.Unlock()
	return r
}

// sync rebuilds the clients when the configuration changed, and only then.
// Rebuilding on every call would throw away warm inventory caches.
func (r *Registry) sync() []Project {
	projects, skipped := ParseProjectsReport(
		r.cfg.Get(settings.HetznerTokens),
		r.cfg.Get(settings.HetznerProjects),
		LegacySettings{
			Token:      r.cfg.Get(settings.HetznerToken),
			SnapshotID: r.cfg.Get(settings.HetznerSnapshotID),
			ServerType: r.cfg.Get(settings.HetznerServerType),
			SSHKeys:    r.cfg.Get(settings.HetznerSSHKeys),
			Locations:  r.legacyLocations(),
		})

	// Said once per distinct problem rather than on every call, because sync
	// runs constantly. Without it a project configured with no token vanishes
	// and the only trace is an empty project list at startup.
	r.reportSkipped(skipped)

	fp := Fingerprint(projects)
	r.mu.Lock()
	defer r.mu.Unlock()
	if fp == r.fingerprint {
		return r.projects
	}

	clients := make(map[string]*Client, len(projects))
	for _, p := range projects {
		slug := p.Slug
		c := New(r.cfg, r.log, r.timeout, r.ttl).withToken(func() string {
			return r.tokenFor(slug)
		})
		if r.base != "" {
			c = c.WithBaseURL(r.base)
		}
		clients[slug] = c
	}
	r.projects, r.clients, r.fingerprint = projects, clients, fp
	return projects
}

// reportSkipped warns about unusable projects, once per distinct set, so a
// misconfiguration is visible without filling the log.
func (r *Registry) reportSkipped(skipped []string) {
	joined := strings.Join(skipped, "; ")
	r.mu.Lock()
	seen := r.skippedReport == joined
	r.skippedReport = joined
	r.mu.Unlock()
	if seen || joined == "" {
		return
	}
	r.log.Warn("some Hetzner projects are configured but unusable",
		"skipped", joined,
		"impact", "nothing can be built or deleted in them")
}

// legacyLocations keeps the single-project settings working unchanged: the
// ordered preference if it is set, and otherwise every location the old
// country-to-location map mentions.
func (r *Registry) legacyLocations() string {
	if ordered := strings.TrimSpace(r.cfg.Get(settings.ProvisionLocations)); ordered != "" {
		return ordered
	}

	seen := map[string]bool{}
	var out []string
	for _, pair := range strings.Split(r.cfg.Get(settings.HetznerLocations), ",") {
		_, location, ok := strings.Cut(strings.TrimSpace(pair), ":")
		if !ok {
			continue
		}
		if location = strings.TrimSpace(location); location != "" && !seen[location] {
			seen[location] = true
			out = append(out, location)
		}
	}
	// The single-location setting is the last resort, after any explicit
	// preference, so adding it never reorders what was already configured.
	if single := strings.TrimSpace(r.cfg.Get(settings.HetznerLocation)); single != "" && !seen[single] {
		out = append(out, single)
	}
	return strings.Join(out, ",")
}

// tokenFor re-reads one project's token, so a rotation takes effect without a
// restart the same way the single-token setting always did.
func (r *Registry) tokenFor(slug string) string {
	for _, p := range ParseProjects(
		r.cfg.Get(settings.HetznerTokens),
		r.cfg.Get(settings.HetznerProjects),
		LegacySettings{Token: r.cfg.Get(settings.HetznerToken)},
	) {
		if p.Slug == slug {
			return p.Token
		}
	}
	return ""
}

// Projects lists what is configured, in order.
func (r *Registry) Projects() []Project { return r.sync() }

// Slugs names the configured projects.
func (r *Registry) Slugs() []string { return Slugs(r.sync()) }

// Configured reports whether anything can be reached at all.
func (r *Registry) Configured() bool { return len(r.sync()) > 0 }

// Project returns one project's configuration.
func (r *Registry) Project(slug string) (Project, bool) {
	for _, p := range r.sync() {
		if p.Slug == slug {
			return p, true
		}
	}
	return Project{}, false
}

// Client returns the client bound to one project. Everything that creates or
// deletes must go through this rather than picking a client by convenience.
func (r *Registry) Client(slug string) (*Client, bool) {
	r.sync()
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.clients[slug]
	return c, ok
}

// Default is the first configured project, for the read-only paths that only
// need "some" client. It is deliberately not used for create or delete.
func (r *Registry) Default() *Client {
	projects := r.sync()
	if len(projects) == 0 {
		return nil
	}
	c, _ := r.Client(projects[0].Slug)
	return c
}

// Invalidate drops one project's cached inventory.
func (r *Registry) Invalidate(slug string) {
	if c, ok := r.Client(slug); ok {
		c.InvalidateInventory()
	}
}

// InvalidateAll drops every cached inventory.
func (r *Registry) InvalidateAll() {
	r.sync()
	r.mu.Lock()
	clients := make([]*Client, 0, len(r.clients))
	for _, c := range r.clients {
		clients = append(clients, c)
	}
	r.mu.Unlock()
	for _, c := range clients {
		c.InvalidateInventory()
	}
}

// InventoryAll reads every project. Both maps are returned: a caller that
// writes ownership must know which projects failed, because "not in the
// inventory I could read" is not the same as "not yours".
func (r *Registry) InventoryAll(ctx context.Context) (map[string]*Inventory, map[string]error) {
	invs := map[string]*Inventory{}
	errs := map[string]error{}

	for _, p := range r.sync() {
		c, ok := r.Client(p.Slug)
		if !ok {
			continue
		}
		inv, err := c.Inventory(ctx)
		if err != nil {
			errs[p.Slug] = err
			continue
		}
		invs[p.Slug] = inv
	}
	return invs, errs
}

// Located says which project owns an address.
type Located struct {
	Project   string
	Resource  Resource
	Ownership Ownership
}

// Locate finds the project holding an address.
//
// It refuses to answer rather than guess. Two projects claiming the same
// address, or any project that could not be read, both produce OwnedUnknown —
// because the alternative is telling a caller an address is external, or is
// owned by the wrong project, and both of those get acted on.
func (r *Registry) Locate(ctx context.Context, address string) (Located, error) {
	invs, errs := r.InventoryAll(ctx)

	var hits []Located
	for slug, inv := range invs {
		if res, ok := inv.Lookup(address); ok {
			hits = append(hits, Located{Project: slug, Resource: res, Ownership: OwnedHetzner})
		}
	}

	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		if len(errs) > 0 {
			// A partial view cannot prove absence.
			return Located{Ownership: OwnedUnknown}, firstError(errs)
		}
		if len(invs) == 0 {
			return Located{Ownership: OwnedUnknown}, ErrNotConfigured
		}
		return Located{Ownership: OwnedExternal}, nil
	default:
		slugs := make([]string, 0, len(hits))
		for _, h := range hits {
			slugs = append(slugs, h.Project)
		}
		r.log.Error("an address is claimed by more than one project — refusing to choose",
			"address", address, "projects", slugs)
		return Located{Ownership: OwnedUnknown}, ErrAmbiguousOwner
	}
}

// Capacity is what one project can still hold.
type Capacity struct {
	Servers int
	Limit   int
	Err     error
}

// Full reports whether the project is known to be out of room. An unknown limit
// is never reported as full: capacity is then only discovered by trying.
func (c Capacity) Full() bool { return c.Err == nil && c.Limit > 0 && c.Servers >= c.Limit }

// Capacity reads how many servers each project holds.
func (r *Registry) Capacity(ctx context.Context) map[string]Capacity {
	out := map[string]Capacity{}
	for _, p := range r.sync() {
		c, ok := r.Client(p.Slug)
		if !ok {
			continue
		}
		inv, err := c.Inventory(ctx)
		if err != nil {
			out[p.Slug] = Capacity{Limit: p.MaxServers, Err: err}
			continue
		}
		out[p.Slug] = Capacity{Servers: inv.Servers, Limit: p.MaxServers}
	}
	return out
}

func firstError(errs map[string]error) error {
	for _, err := range errs {
		return err
	}
	return nil
}
