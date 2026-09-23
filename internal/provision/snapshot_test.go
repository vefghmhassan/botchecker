package provision

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/i18n"
	"github.com/vefgh/botchecker/internal/store"
)

func snap(id int64, status string, created string) hetzner.Snapshot {
	t, _ := time.Parse("2006-01-02", created)
	return hetzner.Snapshot{ID: id, Status: status, Created: t, Description: "vless"}
}

func TestTheConfiguredSnapshotIsUsedWhenTheProjectHoldsIt(t *testing.T) {
	got, err := pickSnapshot([]hetzner.Snapshot{
		snap(1, "available", "2026-09-01"),
		snap(2, "available", "2026-09-20"),
	}, "1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "1" || got.Auto {
		t.Errorf("got %+v, want the configured snapshot 1, not an automatic pick", got)
	}
}

// A snapshot deleted in the console used to fail every build with "image not
// found", in every location, and read like an outage.
func TestAMissingConfiguredSnapshotFallsBackToTheNewest(t *testing.T) {
	got, err := pickSnapshot([]hetzner.Snapshot{
		snap(5, "available", "2026-08-01"),
		snap(7, "available", "2026-09-22"),
		snap(6, "available", "2026-09-10"),
	}, "423792178")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "7" || !got.Auto || got.Configured != "423792178" {
		t.Errorf("got %+v, want the newest snapshot 7 chosen automatically", got)
	}
}

func TestNoConfiguredSnapshotPicksTheNewest(t *testing.T) {
	got, err := pickSnapshot([]hetzner.Snapshot{
		snap(5, "available", "2026-08-01"),
		snap(7, "available", "2026-09-22"),
	}, "")
	if err != nil || got.ID != "7" || !got.Auto {
		t.Errorf("got %+v, %v", got, err)
	}
}

// A snapshot still being written cannot be built from, even if it is newest.
func TestASnapshotStillBeingCreatedIsNeverChosen(t *testing.T) {
	got, err := pickSnapshot([]hetzner.Snapshot{
		snap(5, "available", "2026-08-01"),
		snap(9, "creating", "2026-09-23"),
	}, "9")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "5" {
		t.Errorf("got %s, want 5: snapshot 9 is still being created", got.ID)
	}
}

func TestAProjectWithNoSnapshotsSaysSo(t *testing.T) {
	_, err := pickSnapshot([]hetzner.Snapshot{snap(9, "creating", "2026-09-23")}, "")
	if !errors.Is(err, ErrNoSnapshot) {
		t.Errorf("err = %v, want ErrNoSnapshot", err)
	}
}

// The build is given the snapshot the project actually has, not the stale id
// from the settings.
func TestBuildCandidatesAreGivenTheResolvedSnapshot(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/images", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"images": []map[string]any{
				{"id": 11, "status": "available", "created": "2026-09-01T00:00:00Z"},
				{"id": 12, "status": "available", "created": "2026-09-22T00:00:00Z"},
			},
			"meta": map[string]any{"pagination": map[string]any{"next_page": nil}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m, _, _ := testManager(t, srv.URL)
	m.beginProgress("198.51.100.1")

	cands, skipped := m.buildCandidates()
	if len(cands) == 0 {
		t.Fatalf("no candidates: %v", skipped)
	}
	got, skipped := m.withSnapshots(context.Background(), cands)
	if len(got) != len(cands) || len(skipped) != 0 {
		t.Fatalf("got %d candidates, skipped %v", len(got), skipped)
	}
	for _, c := range got {
		if c.SnapshotID != "12" {
			t.Errorf("candidate %s/%s builds from %s, want 12", c.Project, c.Location, c.SnapshotID)
		}
	}

	// And the person watching is told why it is not the configured one.
	steps := m.Progress().Steps
	if len(steps) < 2 || steps[len(steps)-1].Key != StepSnapshotAuto {
		t.Errorf("steps = %+v, want a snapshot-auto step", steps)
	}
}

// Every line the plan and the progress log can produce must have text in both
// languages, or the reader sees a bare key.
func TestEveryPlanAndStepKeyIsTranslated(t *testing.T) {
	keys := map[string]bool{}
	pattern := regexp.MustCompile(`"((?:plan|prog)\.[a-z.]+)"`)
	for _, file := range []string{"plan.go", "progress.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range pattern.FindAllStringSubmatch(string(src), -1) {
			keys[m[1]] = true
		}
	}
	if len(keys) < 30 {
		t.Fatalf("found only %d keys; the pattern no longer matches the source", len(keys))
	}
	fa := map[string]bool{}
	for _, k := range i18n.PersianKeys() {
		fa[k] = true
	}
	for k := range keys {
		if !i18n.Has(k) {
			t.Errorf("%s has no English text", k)
		}
		if !fa[k] {
			t.Errorf("%s has no Persian text", k)
		}
	}
}

// A second press while a run is going must be refused, not queued: two runs
// would buy two servers for the same machine.
func TestASecondRunIsRefusedWhileOneIsGoing(t *testing.T) {
	m, _, _ := testManager(t, "http://127.0.0.1:1")
	m.running = true
	if err := m.TriggerAsync(context.Background(), store.AddressStatus{Address: "198.51.100.1", Ports: []store.TargetStatus{{}}}, 0, Override{Manual: true}); !errors.Is(err, ErrBusy) {
		t.Errorf("err = %v, want ErrBusy", err)
	}
}
