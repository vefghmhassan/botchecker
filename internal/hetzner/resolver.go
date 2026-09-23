package hetzner

import (
	"context"

	"github.com/vefgh/botchecker/internal/scanner"
)

// Resolver adapts the client to the scanner's ProviderResolver, so the scanner
// can tag each address without depending on Hetzner.
type Resolver struct{ c *Client }

func NewResolver(c *Client) *Resolver { return &Resolver{c: c} }

// Resolve classifies one address. With no token configured the answer is
// "unknown" rather than "external": we genuinely cannot tell, and claiming an
// address is not yours would be worse than admitting ignorance.
func (r *Resolver) Resolve(ctx context.Context, address string) scanner.ProviderInfo {
	if r == nil || r.c == nil || !r.c.Configured() {
		return scanner.ProviderInfo{Provider: string(OwnedUnknown)}
	}

	inv, err := r.c.Inventory(ctx)
	if err != nil {
		return scanner.ProviderInfo{Provider: string(OwnedUnknown)}
	}

	res, ok := inv.Lookup(address)
	if !ok {
		return scanner.ProviderInfo{Provider: string(OwnedExternal)}
	}
	return scanner.ProviderInfo{
		Provider:     string(OwnedHetzner),
		Kind:         res.Kind,
		ServerID:     res.ServerID,
		ServerName:   res.ServerName,
		AbuseBlocked: res.AbuseBlocked,
	}
}

// MultiResolver answers ownership across every configured project.
//
// It is one resolver over N projects rather than N resolvers, so the scanner
// keeps a single ProviderResolver and never learns that projects exist.
type MultiResolver struct{ reg *Registry }

func NewMultiResolver(reg *Registry) *MultiResolver { return &MultiResolver{reg: reg} }

func (m *MultiResolver) Resolve(ctx context.Context, address string) scanner.ProviderInfo {
	if m.reg == nil || !m.reg.Configured() {
		return scanner.ProviderInfo{Provider: string(OwnedUnknown)}
	}

	located, err := m.reg.Locate(ctx, address)
	if err != nil {
		// Unknown, never external: a project that could not be read is not
		// proof the address belongs to someone else.
		return scanner.ProviderInfo{Provider: string(OwnedUnknown)}
	}
	if located.Ownership != OwnedHetzner {
		return scanner.ProviderInfo{Provider: string(located.Ownership)}
	}

	return scanner.ProviderInfo{
		Provider:     string(OwnedHetzner),
		Project:      located.Project,
		Kind:         located.Resource.Kind,
		ServerID:     located.Resource.ServerID,
		ServerName:   located.Resource.ServerName,
		AbuseBlocked: located.Resource.AbuseBlocked,
	}
}
