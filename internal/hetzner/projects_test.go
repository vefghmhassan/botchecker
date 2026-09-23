package hetzner

import (
	"strings"
	"testing"
)

// An install that never touches the new settings must behave exactly as it did
// with one token, or upgrading would quietly stop replacements working.
func TestNoProjectsConfiguredFallsBackToTheSingleToken(t *testing.T) {
	got := ParseProjects("", "", LegacySettings{
		Token: "legacy-token", SnapshotID: "423792178",
		ServerType: "cpx31", Locations: "ash,hil",
	})

	if len(got) != 1 {
		t.Fatalf("got %d projects, want 1", len(got))
	}
	p := got[0]
	if p.Slug != "default" || p.Token != "legacy-token" || p.SnapshotID != "423792178" {
		t.Fatalf("got %+v", p)
	}
	if ok, why := p.Usable(); !ok {
		t.Errorf("the legacy project cannot build: %s", why)
	}
}

func TestNoTokenAtAllIsNoProjects(t *testing.T) {
	if got := ParseProjects("", "", LegacySettings{}); len(got) != 0 {
		t.Fatalf("got %+v, want nothing configured", got)
	}
}

// A single pasted token with no name still has to work.
func TestABareTokenBecomesTheDefaultProject(t *testing.T) {
	got := ParseProjects("just-a-token", "", LegacySettings{})
	if len(got) != 1 || got[0].Slug != "default" || got[0].Token != "just-a-token" {
		t.Fatalf("got %+v", got)
	}
}

func TestProjectsCarryTheirOwnResources(t *testing.T) {
	got := ParseProjects(
		"main=tok-a, spare=tok-b",
		"main|snapshot=111|type=cpx32|types=ash:cpx31,hil:cpx31|locations=ash,hel1|max=5;"+
			"spare|snapshot=222|type=cpx31|locations=ash",
		LegacySettings{})

	if len(got) != 2 {
		t.Fatalf("got %d projects, want 2", len(got))
	}
	main := got[0]
	if main.Slug != "main" || main.SnapshotID != "111" || main.MaxServers != 5 {
		t.Fatalf("main = %+v", main)
	}
	// A server type is not offered everywhere: cpx31 is US-only and cpx32 is
	// its European equivalent, so the location decides.
	if got := main.ServerTypeFor("ash"); got != "cpx31" {
		t.Errorf("ash type = %q, want cpx31", got)
	}
	if got := main.ServerTypeFor("hel1"); got != "cpx32" {
		t.Errorf("hel1 type = %q, want the project default cpx32", got)
	}
	if got[1].SnapshotID != "222" {
		t.Errorf("spare kept main's snapshot: %+v", got[1])
	}
}

// With no snapshot configured the project can still build: the newest snapshot
// it holds is looked up at build time, so leaving the id blank is a choice
// rather than a misconfiguration.
func TestAProjectWithoutASnapshotCanStillBuild(t *testing.T) {
	got := ParseProjects("spare=tok", "spare|locations=ash|type=cpx31", LegacySettings{})
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if ok, why := got[0].Usable(); !ok {
		t.Fatalf("a project with no snapshot was refused: %s", why)
	}
}

// Naming a project in the resources blob without giving it a token would leave
// something that looks configured but can never be reached.
func TestAProjectWithoutATokenIsDropped(t *testing.T) {
	got := ParseProjects("main=tok", "main|snapshot=1;ghost|snapshot=2", LegacySettings{})
	if len(got) != 1 || got[0].Slug != "main" {
		t.Fatalf("got %+v, want only the project that has a token", Slugs(got))
	}
}

// Adding a second token must not silently strip the first project of the
// settings it was already using.
func TestAProjectWithNoResourcesInheritsTheLegacySettings(t *testing.T) {
	got := ParseProjects("main=tok-a, spare=tok-b", "",
		LegacySettings{SnapshotID: "423792178", ServerType: "cpx31", Locations: "ash"})

	for _, p := range got {
		if p.SnapshotID != "423792178" || p.ServerType != "cpx31" {
			t.Errorf("%s lost the existing settings: %+v", p.Slug, p)
		}
	}
}

// A project configured with no token used to disappear without a word, leaving
// an operator staring at an empty project list. On 2026-09-21 three projects
// were configured that way and the service silently did nothing at all.
func TestAProjectWithNoTokenSaysWhyItWasDropped(t *testing.T) {
	got, skipped := ParseProjectsReport(
		"proxy=tok",
		"proxy|snapshot=1|locations=hel1;main|snapshot=2|locations=ash;ctl|snapshot=3",
		LegacySettings{})

	if len(got) != 1 || got[0].Slug != "proxy" {
		t.Fatalf("only the project with a token is usable, got %v", Slugs(got))
	}
	if len(skipped) != 2 {
		t.Fatalf("want a reason for each dropped project, got %v", skipped)
	}
	// The reason has to name the project and what to do, or it is no better
	// than silence.
	joined := strings.Join(skipped, " ")
	for _, want := range []string{"main", "ctl", "token"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the reasons do not mention %q: %v", want, skipped)
		}
	}
}

// The legacy single token is only a fallback for an install that never touched
// the projects setting. Once projects are named, a bare token must not be
// silently attached to one of them — it would be a guess about which account
// owns which machines, and deletes go to accounts.
func TestALegacyTokenDoesNotAdoptNamedProjects(t *testing.T) {
	got, skipped := ParseProjectsReport("", "proxy|snapshot=1", LegacySettings{Token: "legacy"})
	if len(got) != 0 {
		t.Fatalf("a named project was given a token it was never assigned: %v", Slugs(got))
	}
	if len(skipped) != 1 {
		t.Fatalf("want one reason, got %v", skipped)
	}

	// With nothing named, the fallback still works exactly as before.
	got, _ = ParseProjectsReport("", "", LegacySettings{Token: "legacy", SnapshotID: "9"})
	if len(got) != 1 || got[0].Slug != "default" || got[0].Token != "legacy" {
		t.Fatalf("the single-token install stopped working: %v", got)
	}
}

// The dashboard row editor writes these two strings and nothing else, so a
// project edited through a form and one edited through the environment must end
// up identical. If this drifts, saving an untouched form would silently change
// the configuration.
func TestProjectsSurviveARoundTrip(t *testing.T) {
	const (
		tokens = "proxy=tok-a,main=tok-b,ctl=tok-c"
		// Every field the format supports, including the two that only exist
		// because a server type is not offered in every location.
		resources = "proxy|snapshot=423792178|type=cpx31|types=hel1:cpx32,nbg1:cpx32|locations=ash,hil|max=5;" +
			"main|snapshot=433673345|type=cpx42|locations=hel1,fsn1,nbg1|max=5;" +
			"ctl|snapshot=423120548|type=cpx32|types=ash:cpx31|ssh=key-1,key-2|locations=hel1,fsn1|max=5"
	)

	parsed := ParseProjects(tokens, resources, LegacySettings{})
	if len(parsed) != 3 {
		t.Fatalf("parsed %d projects, want 3", len(parsed))
	}

	gotTokens, gotResources := FormatProjects(parsed)
	if gotTokens != tokens {
		t.Errorf("tokens round-tripped to\n  %q\nwant\n  %q", gotTokens, tokens)
	}
	if gotResources != resources {
		t.Errorf("resources round-tripped to\n  %q\nwant\n  %q", gotResources, resources)
	}

	// And parsing the output again gives the same projects, which is what
	// actually matters to everything downstream.
	again := ParseProjects(gotTokens, gotResources, LegacySettings{})
	if Fingerprint(again) != Fingerprint(parsed) {
		t.Errorf("a second round trip changed the projects:\n  %v\n  %v",
			Slugs(parsed), Slugs(again))
	}
}

func TestFormattingSkipsWhatWasNeverSet(t *testing.T) {
	tokens, resources := FormatProjects([]Project{{
		Slug: "proxy", Token: "tok", SnapshotID: "1",
	}})
	if tokens != "proxy=tok" {
		t.Errorf("tokens = %q", tokens)
	}
	// No empty "type=|locations=|max=0" noise: an unset field is absent, so the
	// stored string stays readable and diffable.
	if resources != "proxy|snapshot=1" {
		t.Errorf("resources = %q, want only what was set", resources)
	}
}

func TestFormattingIsStableAcrossSaves(t *testing.T) {
	// Map iteration order must not leak into the stored value, or every save
	// would rewrite the setting and the audit trail would be noise.
	p := []Project{{
		Slug: "proxy", Token: "t", SnapshotID: "1", ServerType: "cpx31",
		TypeByLocation: map[string]string{
			"sin": "cpx32", "hel1": "cpx32", "nbg1": "cpx32", "ash": "cpx31",
		},
	}}
	first, firstRes := FormatProjects(p)
	for i := 0; i < 20; i++ {
		gotTokens, gotRes := FormatProjects(p)
		if gotTokens != first || gotRes != firstRes {
			t.Fatalf("formatting is not stable:\n  %q\n  %q", firstRes, gotRes)
		}
	}
}

func TestARowWithNoTokenStillKeepsItsResources(t *testing.T) {
	// The form allows a row whose token is left blank, meaning "keep the stored
	// one". Formatting must not lose the rest of that row.
	tokens, resources := FormatProjects([]Project{
		{Slug: "proxy", Token: "tok", SnapshotID: "1"},
		{Slug: "main", SnapshotID: "2", Locations: []string{"hel1"}},
	})
	if tokens != "proxy=tok" {
		t.Errorf("tokens = %q, want only the row that has one", tokens)
	}
	if resources != "proxy|snapshot=1;main|snapshot=2|locations=hel1" {
		t.Errorf("resources = %q, the tokenless row lost its settings", resources)
	}
}
