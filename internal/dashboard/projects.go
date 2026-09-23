package dashboard

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/settings"
)

// The Hetzner projects editor.
//
// These used to be two raw strings in the settings form — "proxy=tok,main=tok"
// and "proxy|snapshot=…|locations=…;main|…" — which is a format nobody should
// have to hand-write. On 2026-09-22 the tokens half went missing and all three
// projects were silently dropped, so nothing could be built or deleted at all
// and the only sign was an empty project list at startup.
//
// The storage does not change: rows are serialised back into the same two
// settings, so everything downstream and both environment variables keep
// working. This is only a better way to write them.

// projectRow is one project as the form shows it.
type projectRow struct {
	Slug       string
	SnapshotID string
	ServerType string
	// TypesText is the per-location override, "hel1:cpx32,nbg1:cpx32". A type
	// is not offered in every location — cpx31 is US-only, cpx32 its European
	// equivalent — so one global type cannot serve every site.
	TypesText     string
	SSHText       string
	LocationsText string
	MaxServers    int

	// TokenSet and TokenHint describe the stored token without revealing it.
	TokenSet  bool
	TokenHint string
	// Usable and Why explain at a glance why a row cannot build.
	Usable bool
	Why    string
}

func (h *Handler) projectsPage(c *fiber.Ctx) error {
	if h.d.Settings == nil || h.d.Hetzner == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "settings are not available")
	}

	data := h.base("Projects", "projects")
	data["Rows"] = h.projectRows()
	data["Flash"] = c.Query("msg")
	data["FlashErr"] = c.Query("err")
	return c.Render("projects", data, "layout")
}

// projectRows renders what is configured now, including projects that were
// dropped for having no token — those are exactly the ones a reader needs to
// see, and the parsed list leaves them out by design.
func (h *Handler) projectRows() []projectRow {
	tokens := h.d.Settings.Get(settings.HetznerTokens)
	resources := h.d.Settings.Get(settings.HetznerProjects)

	byToken := map[string]string{}
	for _, pair := range strings.Split(tokens, ",") {
		slug, token, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || strings.TrimSpace(token) == "" {
			continue
		}
		byToken[strings.ToLower(strings.TrimSpace(slug))] = strings.TrimSpace(token)
	}

	// Parsed with no legacy fallback, so what is shown is what is actually
	// stored rather than a value inherited from the single-project settings.
	parsed := hetzner.ParseProjects(tokens, resources, hetzner.LegacySettings{})
	bySlug := map[string]hetzner.Project{}
	for _, p := range parsed {
		bySlug[p.Slug] = p
	}

	var rows []projectRow
	for _, block := range strings.Split(resources, ";") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		slug := strings.ToLower(strings.TrimSpace(strings.SplitN(block, "|", 2)[0]))
		if slug == "" {
			continue
		}
		p, known := bySlug[slug]
		if !known {
			// No token, so ParseProjects dropped it. Re-read it on its own so
			// the row still shows what it holds and why it cannot be used.
			one := hetzner.ParseProjects(slug+"=placeholder", block, hetzner.LegacySettings{})
			if len(one) == 1 {
				p = one[0]
				p.Token = ""
			} else {
				p = hetzner.Project{Slug: slug}
			}
		}
		rows = append(rows, toRow(p, byToken[slug]))
	}

	// A token with no resources block at all would otherwise be invisible.
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Slug] = true
	}
	extra := make([]string, 0, len(byToken))
	for slug := range byToken {
		if !seen[slug] {
			extra = append(extra, slug)
		}
	}
	sort.Strings(extra)
	for _, slug := range extra {
		rows = append(rows, toRow(hetzner.Project{Slug: slug, Token: byToken[slug]}, byToken[slug]))
	}
	return rows
}

func toRow(p hetzner.Project, token string) projectRow {
	r := projectRow{
		Slug:          p.Slug,
		SnapshotID:    p.SnapshotID,
		ServerType:    p.ServerType,
		SSHText:       strings.Join(p.SSHKeys, ","),
		LocationsText: strings.Join(p.Locations, ","),
		MaxServers:    p.MaxServers,
		TokenSet:      token != "",
	}
	if len(p.TypeByLocation) > 0 {
		locs := make([]string, 0, len(p.TypeByLocation))
		for loc := range p.TypeByLocation {
			locs = append(locs, loc)
		}
		sort.Strings(locs)
		pairs := make([]string, 0, len(locs))
		for _, loc := range locs {
			pairs = append(pairs, loc+":"+p.TypeByLocation[loc])
		}
		r.TypesText = strings.Join(pairs, ",")
	}
	if r.TokenSet {
		r.TokenHint = tokenHint(token)
	}
	// Asked of the project as stored, including its token, so the reason shown
	// is the real one.
	p.Token = token
	r.Usable, r.Why = p.Usable()
	return r
}

// tokenHint shows enough of a token to tell two apart, and no more.
func tokenHint(token string) string {
	if len(token) <= 4 {
		return "…"
	}
	return "…" + token[len(token)-4:]
}

// saveProjects rebuilds both settings from the submitted rows.
//
// A blank token means "keep the stored one" — the same rule the settings form
// uses for every secret. Without it, editing a row's locations would wipe the
// token that row depends on, which is a mistake nobody could see afterwards
// because the field is write-only.
func (h *Handler) saveProjects(c *fiber.Ctx) error {
	if h.d.Settings == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "settings are not available")
	}

	existing := map[string]string{}
	for _, pair := range strings.Split(h.d.Settings.Get(settings.HetznerTokens), ",") {
		slug, token, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok {
			existing[strings.ToLower(strings.TrimSpace(slug))] = strings.TrimSpace(token)
		}
	}

	var projects []hetzner.Project
	seen := map[string]bool{}

	for i := 0; ; i++ {
		slug := strings.ToLower(strings.TrimSpace(c.FormValue(fmt.Sprintf("slug_%d", i))))
		if slug == "" {
			// Rows are submitted with dense indexes, but a deleted row leaves a
			// gap; keep looking a little past the first empty one.
			if i > 64 {
				break
			}
			continue
		}
		if seen[slug] {
			return redirectProjects(c, "", fmt.Sprintf("two rows are both called %q", slug))
		}
		seen[slug] = true

		token := strings.TrimSpace(c.FormValue(fmt.Sprintf("token_%d", i)))
		if token == "" {
			token = existing[slug]
		}

		p := hetzner.Project{
			Slug:           slug,
			Token:          token,
			SnapshotID:     strings.TrimSpace(c.FormValue(fmt.Sprintf("snapshot_%d", i))),
			ServerType:     strings.TrimSpace(c.FormValue(fmt.Sprintf("type_%d", i))),
			TypeByLocation: parsePairs(c.FormValue(fmt.Sprintf("types_%d", i))),
			SSHKeys:        splitList(c.FormValue(fmt.Sprintf("ssh_%d", i))),
			Locations:      splitList(c.FormValue(fmt.Sprintf("locations_%d", i))),
		}
		if n, err := strconv.Atoi(strings.TrimSpace(c.FormValue(fmt.Sprintf("max_%d", i)))); err == nil {
			p.MaxServers = n
		}
		projects = append(projects, p)
	}

	tokens, resources := hetzner.FormatProjects(projects)
	if err := h.d.Settings.Set(settings.HetznerProjects, resources); err != nil {
		return redirectProjects(c, "", err.Error())
	}
	if tokens == "" {
		// Clearing rather than writing an empty string, so the setting reads as
		// unset rather than as a value that parses to nothing.
		if err := h.d.Settings.Clear(settings.HetznerTokens); err != nil {
			return redirectProjects(c, "", err.Error())
		}
	} else if err := h.d.Settings.Set(settings.HetznerTokens, tokens); err != nil {
		return redirectProjects(c, "", err.Error())
	}

	h.d.Hetzner.InvalidateAll()

	usable := 0
	for _, p := range hetzner.ParseProjects(tokens, resources, hetzner.LegacySettings{}) {
		if ok, _ := p.Usable(); ok {
			usable++
		}
	}
	return redirectProjects(c, fmt.Sprintf("Saved. %s can build.", plural(usable, "project")), "")
}

// identifyProject says which project a token actually belongs to.
//
// A token sees only its own project, and a snapshot exists only inside the
// project that holds it — so the snapshots a token can list name the project
// beyond doubt. That is the question this answers: paste a token, find out
// which row it belongs in, instead of guessing and discovering the mistake when
// a delete goes to the wrong account.
//
// Read-only. It lists snapshots and servers and nothing else.
func (h *Handler) identifyProject(c *fiber.Ctx) error {
	if h.d.Hetzner == nil {
		return redirectProjects(c, "", "Hetzner is not configured.")
	}
	slug := strings.ToLower(strings.TrimSpace(c.FormValue("slug")))
	if slug == "" {
		return redirectProjects(c, "", "Which row?")
	}
	cli, ok := h.d.Hetzner.Client(slug)
	if !ok {
		return redirectProjects(c, "", fmt.Sprintf("%q has no usable token yet — save one first.", slug))
	}

	ctx, cancel := contextWithTimeout(c, 30*time.Second)
	defer cancel()

	snaps, err := cli.Snapshots(ctx)
	if err != nil {
		return redirectProjects(c, "", fmt.Sprintf("%s: %s", slug, err))
	}

	// Which of the configured snapshot ids this token can actually see. That is
	// the identification: any match names the project the token belongs to.
	configured := map[string]string{}
	for _, p := range hetzner.ParseProjects(
		h.d.Settings.Get(settings.HetznerTokens),
		h.d.Settings.Get(settings.HetznerProjects),
		hetzner.LegacySettings{}) {
		if p.SnapshotID != "" {
			configured[p.SnapshotID] = p.Slug
		}
	}

	var matches []string
	for _, s := range snaps {
		id := strconv.FormatInt(s.ID, 10)
		if owner, ok := configured[id]; ok {
			matches = append(matches, fmt.Sprintf("%s (%s)", id, owner))
		}
	}

	msg := fmt.Sprintf("%s: %s visible", slug, plural(len(snaps), "snapshot"))
	if inv, err := cli.Inventory(ctx); err == nil {
		msg += fmt.Sprintf(", %s", plural(inv.Servers, "server"))
	}
	switch {
	case len(matches) == 0:
		msg += " — none of the configured snapshots are in this project, so this token belongs to a different one"
	default:
		msg += " — holds the snapshot configured for " + strings.Join(matches, ", ")
	}
	return redirectProjects(c, msg, "")
}

func parsePairs(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), ":")
		if !ok {
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		if k != "" && v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func redirectProjects(c *fiber.Ctx, msg, errMsg string) error {
	q := "/projects"
	switch {
	case errMsg != "":
		q += "?err=" + urlEscape(errMsg)
	case msg != "":
		q += "?msg=" + urlEscape(msg)
	}
	return c.Redirect(q, fiber.StatusSeeOther)
}
