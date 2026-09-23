package hetzner

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Project is one Hetzner account's worth of resources.
//
// A token only ever sees its own project, and every id inside one — servers,
// snapshots, SSH keys — is local to it. So a project is not just a credential:
// it is the credential plus the resources that only exist behind it. Keeping
// them together is what stops a build from asking project B to restore project
// A's snapshot, or a delete from firing a server id at the wrong account.
type Project struct {
	Slug       string
	Token      string
	SnapshotID string
	// ServerType is used where TypeByLocation has no entry.
	ServerType string
	// TypeByLocation exists because a type is not available everywhere: cpx31
	// is US-only and cpx32 is its European equivalent, so one global type
	// cannot serve both.
	TypeByLocation map[string]string
	SSHKeys        []string
	Locations      []string
	// MaxServers is the project's server limit, when the operator knows it.
	// Zero means unknown, and capacity is then only discovered by trying.
	MaxServers int
}

// ServerTypeFor picks the type to create in one location.
func (p Project) ServerTypeFor(location string) string {
	if t, ok := p.TypeByLocation[strings.ToLower(strings.TrimSpace(location))]; ok && t != "" {
		return t
	}
	return p.ServerType
}

// Usable reports whether a replacement could actually be built here, and why
// not when it could not. A snapshot is not required: with none configured the
// newest one in the project is used, and a project that holds none is caught
// when the build looks for it. Locations are not required: with none listed, Hetzner
// picks one, which is what the single-project settings always did. A project that cannot build is still used for
// ownership lookups — knowing an address is yours matters even when you cannot
// replace it from that account.
func (p Project) Usable() (bool, string) {
	switch {
	case p.Token == "":
		return false, "no token"
	case p.ServerType == "" && len(p.TypeByLocation) == 0:
		return false, "no server type configured"
	}
	return true, ""
}

// LegacySettings are the single-project values this feature grew out of. When
// no projects are configured they become one project called "default", so an
// install that never touches the new settings behaves exactly as before.
type LegacySettings struct {
	Token, SnapshotID, ServerType, SSHKeys, Locations string
}

// ParseProjects reads the two settings blobs into projects.
//
// Tokens are "slug=token" pairs, comma separated; a bare token with no slug is
// accepted as "default" so pasting a single token still works. Resources are
// "slug|key=value|…", projects separated by ";".
//
//	tokens:   main=AbC…, spare=XyZ…
//	projects: main|snapshot=423792178|type=cpx32|types=ash:cpx31,hil:cpx31|locations=ash,hel1|max=5;
//	          spare|snapshot=99|type=cpx31|locations=ash
//
// The token is kept in its own secret setting so the panel can encrypt it,
// while the resources stay readable and editable in the settings form.
func ParseProjects(tokens, projects string, legacy LegacySettings) []Project {
	out, _ := ParseProjectsReport(tokens, projects, legacy)
	return out
}

// ParseProjectsReport is ParseProjects with the reason for anything left out.
//
// A project named in the resources blob but given no token is unreachable, so
// it is dropped — but dropping it silently is how an account ends up looking
// like it has no projects at all, with nothing in the log to say why. That
// happened on 2026-09-21: three projects were configured, the tokens setting
// was empty, and the service reported "hetzner_projects=" and simply did
// nothing, while a legacy single token sat unused in the environment.
func ParseProjectsReport(tokens, projects string, legacy LegacySettings) ([]Project, []string) {
	bySlug := map[string]*Project{}
	var order []string

	add := func(slug string) *Project {
		if p, ok := bySlug[slug]; ok {
			return p
		}
		p := &Project{Slug: slug, TypeByLocation: map[string]string{}}
		bySlug[slug] = p
		order = append(order, slug)
		return p
	}

	for _, pair := range strings.Split(tokens, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		slug, token, ok := strings.Cut(pair, "=")
		if !ok {
			slug, token = "default", pair
		}
		slug = strings.ToLower(strings.TrimSpace(slug))
		token = strings.TrimSpace(token)
		if slug == "" || token == "" {
			continue
		}
		add(slug).Token = token
	}

	for _, block := range strings.Split(projects, ";") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		fields := strings.Split(block, "|")
		slug := strings.ToLower(strings.TrimSpace(fields[0]))
		if slug == "" {
			continue
		}
		p := add(slug)

		for _, f := range fields[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(f), "=")
			if !ok {
				continue
			}
			key = strings.ToLower(strings.TrimSpace(key))
			value = strings.TrimSpace(value)

			switch key {
			case "snapshot":
				p.SnapshotID = value
			case "type":
				p.ServerType = value
			case "types":
				for _, pair := range strings.Split(value, ",") {
					loc, typ, ok := strings.Cut(strings.TrimSpace(pair), ":")
					if !ok {
						continue
					}
					p.TypeByLocation[strings.ToLower(strings.TrimSpace(loc))] = strings.TrimSpace(typ)
				}
			case "ssh":
				p.SSHKeys = splitCSV(value)
			case "locations":
				p.Locations = splitCSV(value)
			case "max":
				if n, err := strconv.Atoi(value); err == nil && n > 0 {
					p.MaxServers = n
				}
			}
		}
	}

	// Nothing configured: fall back to the single-project settings so an
	// untouched install keeps working exactly as it did.
	if len(order) == 0 {
		if strings.TrimSpace(legacy.Token) == "" {
			return nil, nil
		}
		p := add("default")
		p.Token = strings.TrimSpace(legacy.Token)
		p.SnapshotID = strings.TrimSpace(legacy.SnapshotID)
		p.ServerType = strings.TrimSpace(legacy.ServerType)
		p.SSHKeys = splitCSV(legacy.SSHKeys)
		p.Locations = splitCSV(legacy.Locations)
	}

	out := make([]Project, 0, len(order))
	var skipped []string
	for _, slug := range order {
		p := bySlug[slug]
		// A project named only in the resources blob, with no token, cannot be
		// reached at all. Dropping it here keeps every later stage from having
		// to re-check — but the reason is carried out so the caller can say so.
		if p.Token == "" {
			skipped = append(skipped, fmt.Sprintf(
				"%s has no token: add %q to the Hetzner tokens setting", slug, slug+"=<token>"))
			continue
		}
		// A project with no resources of its own inherits the single-project
		// settings, so adding a second token does not silently break the first.
		if p.SnapshotID == "" {
			p.SnapshotID = strings.TrimSpace(legacy.SnapshotID)
		}
		if p.ServerType == "" {
			p.ServerType = strings.TrimSpace(legacy.ServerType)
		}
		if len(p.SSHKeys) == 0 {
			p.SSHKeys = splitCSV(legacy.SSHKeys)
		}
		if len(p.Locations) == 0 {
			p.Locations = splitCSV(legacy.Locations)
		}
		out = append(out, *p)
	}
	return out, skipped
}

// Slugs lists project names in configured order.
func Slugs(projects []Project) []string {
	out := make([]string, 0, len(projects))
	for _, p := range projects {
		out = append(out, p.Slug)
	}
	return out
}

// Fingerprint identifies a set of projects, so a registry can tell whether its
// clients are still the right ones without rebuilding them on every call.
func Fingerprint(projects []Project) string {
	parts := make([]string, 0, len(projects))
	for _, p := range projects {
		parts = append(parts, fmt.Sprintf("%s:%s:%s:%s:%s",
			p.Slug, p.Token, p.SnapshotID, p.ServerType, strings.Join(p.Locations, ",")))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

func splitCSV(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// FormatProjects is the inverse of ParseProjects: it turns a set of projects
// back into the two settings blobs they are stored as.
//
// The storage shape is deliberately unchanged. A row editor in the dashboard is
// only a nicer way to write these two strings, so everything downstream — and
// the HETZNER_TOKENS / HETZNER_PROJECTS environment variables — keeps working
// exactly as before. Editing a project through a form and editing it through
// the environment must produce the same thing, which the round-trip test is
// there to hold.
//
// The token blob is returned separately because it is the only secret: the
// settings store encrypts that one and leaves the resources readable.
func FormatProjects(projects []Project) (tokens, resources string) {
	var tokenParts, resourceParts []string

	for _, p := range projects {
		slug := strings.ToLower(strings.TrimSpace(p.Slug))
		if slug == "" {
			continue
		}
		if token := strings.TrimSpace(p.Token); token != "" {
			tokenParts = append(tokenParts, slug+"="+token)
		}

		fields := []string{slug}
		if p.SnapshotID != "" {
			fields = append(fields, "snapshot="+p.SnapshotID)
		}
		if p.ServerType != "" {
			fields = append(fields, "type="+p.ServerType)
		}
		if len(p.TypeByLocation) > 0 {
			// Sorted so that saving an unchanged form produces an unchanged
			// string, rather than reordering the value on every save.
			locs := make([]string, 0, len(p.TypeByLocation))
			for loc := range p.TypeByLocation {
				locs = append(locs, loc)
			}
			sort.Strings(locs)

			pairs := make([]string, 0, len(locs))
			for _, loc := range locs {
				pairs = append(pairs, loc+":"+p.TypeByLocation[loc])
			}
			fields = append(fields, "types="+strings.Join(pairs, ","))
		}
		if len(p.SSHKeys) > 0 {
			fields = append(fields, "ssh="+strings.Join(p.SSHKeys, ","))
		}
		if len(p.Locations) > 0 {
			fields = append(fields, "locations="+strings.Join(p.Locations, ","))
		}
		if p.MaxServers > 0 {
			fields = append(fields, "max="+strconv.Itoa(p.MaxServers))
		}
		resourceParts = append(resourceParts, strings.Join(fields, "|"))
	}

	return strings.Join(tokenParts, ","), strings.Join(resourceParts, ";")
}
