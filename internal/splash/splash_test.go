package splash

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

func loadFixture(t *testing.T) *Response {
	t.Helper()
	raw, err := os.ReadFile("testdata/conf.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var r Response
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return &r
}

func TestTargetsDeduplicates(t *testing.T) {
	r := loadFixture(t)

	if got := len(r.Configs()); got != 14 {
		t.Fatalf("Configs() = %d entries, want 14", got)
	}

	targets := r.Targets()
	if len(targets) != 9 {
		var keys []string
		for _, tg := range targets {
			keys = append(keys, tg.Key())
		}
		t.Fatalf("Targets() = %d unique endpoints, want 9: %v", len(targets), keys)
	}
}

func TestTargetsCollectsDuplicateConfigIDs(t *testing.T) {
	targets := loadFixture(t).Targets()

	byKey := map[string]Target{}
	for _, tg := range targets {
		byKey[tg.Key()] = tg
	}

	cases := []struct {
		key     string
		wantIDs []int
	}{
		// The live config lists this endpoint twice, under two config ids.
		{"5.161.158.200:443", []int{30378, 30425}},
		{"77.42.38.98:443", []int{23237, 23267}},
		{"2.29.60.163:443", []int{29750, 29758}},
		{"2.29.50.112:25245", []int{30257}},
	}
	for _, tc := range cases {
		got, ok := byKey[tc.key]
		if !ok {
			t.Errorf("%s missing from targets", tc.key)
			continue
		}
		if !reflect.DeepEqual(got.ConfigIDs, tc.wantIDs) {
			t.Errorf("%s ConfigIDs = %v, want %v", tc.key, got.ConfigIDs, tc.wantIDs)
		}
	}
}

func TestTargetsPreservesPortAndAdsFlag(t *testing.T) {
	byKey := map[string]Target{}
	for _, tg := range loadFixture(t).Targets() {
		byKey[tg.Key()] = tg
	}

	// A non-standard port must survive: probing the wrong port would report a
	// healthy server as broken.
	if got := byKey["2.29.50.112:25245"].Port; got != 25245 {
		t.Errorf("port = %d, want 25245", got)
	}
	if !byKey["62.238.45.47:8443"].Ads {
		t.Error("62.238.45.47:8443 should be flagged as an ads server")
	}
	if byKey["5.161.158.200:443"].Ads {
		t.Error("5.161.158.200:443 should not be flagged as an ads server")
	}
}

func TestTargetsSkipsInvalidEntries(t *testing.T) {
	r := &Response{NoAdsList: []ServerConfig{
		{ID: 1, Address: "1.2.3.4", Port: 443},
		{ID: 2, Address: "", Port: 443},
		{ID: 3, Address: "5.6.7.8", Port: 0},
		{ID: 4, Address: "9.9.9.9", Port: 70000},
	}}
	got := r.Targets()
	if len(got) != 1 || got[0].Key() != "1.2.3.4:443" {
		t.Fatalf("Targets() = %+v, want only 1.2.3.4:443", got)
	}
}

func TestParseExtraTargets(t *testing.T) {
	got := ParseExtraTargets(" 5.161.128.192:443:us , 5.161.128.192:8443 , ,1.2.3.4:99999, bad, 5.161.128.192:443:US ")

	if len(got) != 2 {
		t.Fatalf("parsed %d targets, want 2: %+v", len(got), got)
	}
	if got[0].Address != "5.161.128.192" || got[0].Port != 443 || got[0].CountryCode != "US" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Port != 8443 || got[1].CountryCode != "" {
		t.Errorf("second = %+v", got[1])
	}
	for _, g := range got {
		if !g.Manual {
			t.Errorf("%s is not marked manual", g.Key())
		}
		// Splash never handed these out, so claiming they are active would be a
		// lie the dashboard would repeat.
		if g.Active {
			t.Errorf("%s is marked active", g.Key())
		}
	}
}

func TestParseExtraTargetsRejectsNonsense(t *testing.T) {
	for _, in := range []string{"", "   ", "nonsense", "1.2.3.4", "1.2.3.4:0", "1.2.3.4:-1", "1.2.3.4:abc", ":443", "a:1:2:3"} {
		if got := ParseExtraTargets(in); len(got) != 0 {
			t.Errorf("ParseExtraTargets(%q) = %+v, want none", in, got)
		}
	}
}

// Splash owns an endpoint it returns: it has the config ids, names and country
// that a hand-written entry does not.
func TestMergeTargetsPrefersSplash(t *testing.T) {
	fromSplash := []Target{{
		Address: "1.1.1.1", Port: 443, CountryCode: "FI",
		ConfigIDs: []int{7}, Names: []string{"fi 9-1"}, Active: true,
	}}
	extra := ParseExtraTargets("1.1.1.1:443:US, 2.2.2.2:8443:US")

	merged := MergeTargets(fromSplash, extra)
	if len(merged) != 2 {
		t.Fatalf("merged %d, want 2: %+v", len(merged), merged)
	}
	if merged[0].CountryCode != "FI" || len(merged[0].ConfigIDs) != 1 || merged[0].Manual {
		t.Errorf("splash entry was overwritten: %+v", merged[0])
	}
	if merged[1].Address != "2.2.2.2" || !merged[1].Manual {
		t.Errorf("extra entry = %+v", merged[1])
	}
}

func TestMergeTargetsWithNothingExtraIsUnchanged(t *testing.T) {
	fromSplash := []Target{{Address: "1.1.1.1", Port: 443}}
	if got := MergeTargets(fromSplash, nil); len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
}

// Read from the complete node list, one production endpoint carries 1,002
// config names. Printed whole that is about 12,000 characters, and Telegram
// refuses any message over 4,096 — so the bot's screen for that server would
// simply fail to send.
func TestAThousandConfigNamesFitInOneTelegramMessage(t *testing.T) {
	names := make([]string, 1002)
	for i := range names {
		names[i] = fmt.Sprintf("USA-C-AA-%d", i)
	}

	got := SummariseNames(names, 5)
	if len(got) > 200 {
		t.Fatalf("summary is %d characters: %q", len(got), got)
	}
	// It has to say how many there are, or the reader thinks there are five.
	if !strings.Contains(got, "1002") || !strings.Contains(got, "+997") {
		t.Errorf("the summary hides how many configs there are: %q", got)
	}
	// And a short list is shown as it is.
	if got := SummariseNames([]string{"a", "b"}, 5); got != "a, b" {
		t.Errorf("a short list was changed: %q", got)
	}
}
