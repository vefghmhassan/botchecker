package telebot

import (
	"strings"
	"testing"
	"time"

	"github.com/vefgh/botchecker/internal/i18n"
	"github.com/vefgh/botchecker/internal/provision"
)

// Error text from Hetzner or the panel goes straight into these lines, and one
// stray "<" makes Telegram reject the whole edit — the screen then freezes on
// its last frame with no word of why.
func TestProgressEscapesWhatOutsideServicesSay(t *testing.T) {
	start := time.Date(2026, 9, 23, 20, 0, 0, 0, time.UTC)
	p := provision.Progress{
		Address: "198.51.100.1", Started: start, Finished: start.Add(95 * time.Second),
		Steps: []provision.Step{
			{At: start, Key: provision.StepStart, Args: []any{"198.51.100.1"}},
			{At: start.Add(65 * time.Second), Key: provision.StepCreateFailed, Args: []any{"ash", "<html>limit</html>"}},
		},
		Err: "server said <b>no</b>",
	}
	got := progressText(i18n.EN, p, start.Add(time.Hour))
	if strings.Contains(got, "<html>") || strings.Contains(got, "<b>no</b>") {
		t.Errorf("unescaped text reached Telegram:\n%s", got)
	}
	if !strings.Contains(got, "1:05") {
		t.Errorf("step offsets missing:\n%s", got)
	}
	// Finished runs show their own duration, not the time since.
	if !strings.Contains(got, "1m35s") {
		t.Errorf("elapsed should be the run's length:\n%s", got)
	}
}

func TestPlanNumbersTheChangesAndMarksBlockers(t *testing.T) {
	p := provision.Plan{
		Address: "198.51.100.1",
		Checks: []provision.PlanLine{
			{Level: provision.PlanOK, Key: "plan.panelok"},
			{Level: provision.PlanBlock, Key: "plan.dryrun"},
		},
		Steps: []provision.PlanLine{
			{Key: "plan.step.move", Args: []any{12, "198.51.100.1"}},
			{Key: "plan.step.fail", Args: []any{12}},
		},
		Blocked: true,
	}
	got := planText(i18n.EN, p)
	for _, want := range []string{"✅", "🚫", "1. Move all 12 configs", "2. If nothing works"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
