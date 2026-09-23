package hetzner

import "sort"

// Assignment is what one address should be recorded as.
type Assignment struct {
	Address   string
	Ownership Ownership
	Project   string
	Resource  Resource
}

// Conflict is an address more than one project claims.
type Conflict struct {
	Address  string
	Projects []string
}

// Reconcile decides what every address is, given what each project could see.
//
// The rule that matters is the refusal: if any project failed to answer, no
// address is downgraded to external. "Absent from the inventories I managed to
// read" is not evidence of "not yours", and writing it as such would erase the
// correct ownership of every address in the project that was briefly
// unreachable — after which nothing would replace them, because the service
// only replaces what it believes it owns.
//
// It is a plain function so this can be tested without standing up HTTP.
func Reconcile(addresses []string, invs map[string]*Inventory, errs map[string]error) ([]Assignment, []Conflict) {
	slugs := make([]string, 0, len(invs))
	for slug := range invs {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	partial := len(errs) > 0
	var out []Assignment
	var conflicts []Conflict

	for _, address := range addresses {
		var hits []Assignment
		for _, slug := range slugs {
			if res, ok := invs[slug].Lookup(address); ok {
				hits = append(hits, Assignment{
					Address: address, Ownership: OwnedHetzner, Project: slug, Resource: res,
				})
			}
		}

		switch {
		case len(hits) == 1:
			out = append(out, hits[0])

		case len(hits) > 1:
			// Recorded as ours but with no project, so nothing will act on it
			// until a human resolves which account really holds it.
			names := make([]string, 0, len(hits))
			for _, h := range hits {
				names = append(names, h.Project)
			}
			conflicts = append(conflicts, Conflict{Address: address, Projects: names})
			out = append(out, Assignment{
				Address: address, Ownership: OwnedHetzner, Resource: hits[0].Resource,
			})

		case partial:
			// Leave it alone rather than claim it is someone else's.
			out = append(out, Assignment{Address: address, Ownership: OwnedUnknown})

		default:
			out = append(out, Assignment{Address: address, Ownership: OwnedExternal})
		}
	}
	return out, conflicts
}
