package provision

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/vefgh/botchecker/internal/hetzner"
)

// Choosing the image a replacement is built from.
//
// A snapshot id is configured by hand, and it goes stale the moment someone
// takes a fresh snapshot and deletes the old one in the console. The build then
// fails in every location with "image not found", which reads as a Hetzner
// outage rather than a typo. So the configured id is checked against what the
// project actually holds, and when it is missing the newest usable snapshot is
// taken instead — every server in these projects is built from the same image,
// so the newest one is the one a person would pick.

// SnapshotChoice is the image one project will build from, and why.
type SnapshotChoice struct {
	ID          string
	Description string
	Created     string
	// Auto is true when the configured id was not used, either because none
	// is configured or because the project no longer holds it.
	Auto bool
	// Configured is what the settings asked for, kept for the explanation.
	Configured string
}

// ErrNoSnapshot means the project holds no snapshot a server can be built from.
var ErrNoSnapshot = errors.New("the project holds no available snapshot")

// pickSnapshot applies the rule above to one project's snapshot list.
func pickSnapshot(snaps []hetzner.Snapshot, configured string) (SnapshotChoice, error) {
	var usable []hetzner.Snapshot
	for _, s := range snaps {
		// "creating" is a snapshot still being written; building from it
		// fails. Anything else unknown is treated the same way.
		if s.Status == "" || s.Status == "available" {
			usable = append(usable, s)
		}
	}
	if len(usable) == 0 {
		return SnapshotChoice{Configured: configured}, ErrNoSnapshot
	}

	for _, s := range usable {
		if strconv.FormatInt(s.ID, 10) == configured {
			return choiceFrom(s, configured, false), nil
		}
	}

	sort.SliceStable(usable, func(i, j int) bool { return usable[i].Created.After(usable[j].Created) })
	return choiceFrom(usable[0], configured, true), nil
}

func choiceFrom(s hetzner.Snapshot, configured string, auto bool) SnapshotChoice {
	created := ""
	if !s.Created.IsZero() {
		created = s.Created.UTC().Format("2006-01-02")
	}
	return SnapshotChoice{
		ID: strconv.FormatInt(s.ID, 10), Description: s.Description,
		Created: created, Auto: auto, Configured: configured,
	}
}

// resolveSnapshot asks one project which image to build from.
//
// A failure to list is not a reason to stop: the configured id is kept and the
// build is left to succeed or fail on its own, which is exactly what happened
// before this check existed. Only a project with nothing configured and
// nothing readable has no image at all.
func (m *Manager) resolveSnapshot(ctx context.Context, project, configured string) (SnapshotChoice, error) {
	cli, ok := m.hz.Client(project)
	if !ok {
		return SnapshotChoice{Configured: configured}, fmt.Errorf("project %s is not configured", project)
	}
	snaps, err := cli.Snapshots(ctx)
	if err != nil {
		if configured != "" {
			m.log.Warn("could not list snapshots, building from the configured one",
				"project", project, "snapshot", configured, "err", err)
			return SnapshotChoice{ID: configured, Configured: configured}, nil
		}
		return SnapshotChoice{}, fmt.Errorf("could not list the snapshots of %s: %w", project, err)
	}
	choice, err := pickSnapshot(snaps, configured)
	if err != nil {
		return choice, err
	}
	if choice.Auto {
		m.log.Warn("the configured snapshot is not in the project, using the newest one",
			"project", project, "configured", configured, "using", choice.ID)
	}
	return choice, nil
}

// withSnapshots fills in the image for every candidate, one lookup per
// project, and drops the projects that have none.
func (m *Manager) withSnapshots(ctx context.Context, candidates []buildCandidate) ([]buildCandidate, []string) {
	chosen := map[string]SnapshotChoice{}
	failed := map[string]error{}
	var out []buildCandidate
	var skipped []string

	for _, c := range candidates {
		if _, done := failed[c.Project]; done {
			continue
		}
		choice, ok := chosen[c.Project]
		if !ok {
			var err error
			choice, err = m.resolveSnapshot(ctx, c.Project, c.SnapshotID)
			if err != nil {
				failed[c.Project] = err
				skipped = append(skipped, fmt.Sprintf("%s (%v)", c.Project, err))
				continue
			}
			chosen[c.Project] = choice
			if choice.Auto {
				m.step(StepSnapshotAuto, c.Project, choice.ID, choice.Created, orDash(choice.Configured))
			}
		}
		c.SnapshotID = choice.ID
		out = append(out, c)
	}
	return out, skipped
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
