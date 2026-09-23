package i18n

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every English key must have a Persian counterpart. A missing one would fall
// back to English mid-sentence, which reads worse than a bad translation.
func TestPersianCoversEveryKey(t *testing.T) {
	var missing []string
	for _, k := range Keys() {
		if _, ok := persian[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("%d key(s) have no Persian translation:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// A stray Persian key means a rename left one behind; it would never render.
func TestNoOrphanPersianKeys(t *testing.T) {
	var orphans []string
	for _, k := range PersianKeys() {
		if !Has(k) {
			orphans = append(orphans, k)
		}
	}
	sort.Strings(orphans)
	if len(orphans) > 0 {
		t.Fatalf("%d Persian key(s) have no English original:\n  %s",
			len(orphans), strings.Join(orphans, "\n  "))
	}
}

var verbs = regexp.MustCompile(`%[a-zA-Z]`)

// Format verbs must match between languages, or a translated string would
// print the wrong values — or panic on a missing argument.
func TestFormatVerbsMatch(t *testing.T) {
	for _, k := range Keys() {
		en, fa := english[k], persian[k]
		if fa == "" {
			continue
		}
		gotEN, gotFA := verbs.FindAllString(en, -1), verbs.FindAllString(fa, -1)
		if len(gotEN) != len(gotFA) {
			t.Errorf("%s: English has %v, Persian has %v", k, gotEN, gotFA)
			continue
		}
		for i := range gotEN {
			if gotEN[i] != gotFA[i] {
				t.Errorf("%s: verb %d is %s in English but %s in Persian", k, i, gotEN[i], gotFA[i])
			}
		}
	}
}

func TestParseAndDirection(t *testing.T) {
	cases := map[string]Lang{"fa": FA, "FA": FA, " fa ": FA, "en": EN, "": EN, "de": EN}
	for in, want := range cases {
		if got := Parse(in); got != want {
			t.Errorf("Parse(%q) = %s, want %s", in, got, want)
		}
	}
	if FA.Dir() != "rtl" || EN.Dir() != "ltr" {
		t.Errorf("directions are wrong: fa=%s en=%s", FA.Dir(), EN.Dir())
	}
	if FA.Other() != EN || EN.Other() != FA {
		t.Error("Other() does not toggle")
	}
}

func TestMissingKeyFallsBackVisibly(t *testing.T) {
	// A key nobody defined should render as itself, not as an empty string —
	// a blank label is far harder to notice than a stray identifier.
	if got := T(FA, "no.such.key"); got != "no.such.key" {
		t.Errorf("T(missing) = %q, want the key back", got)
	}
}

func TestPersianFallsBackToEnglishForUntranslated(t *testing.T) {
	english["test.only.english"] = "only here"
	defer delete(english, "test.only.english")

	if got := T(FA, "test.only.english"); got != "only here" {
		t.Errorf("T = %q, want the English text", got)
	}
}

func TestDigitsOnlyConvertsForPersian(t *testing.T) {
	const in = "2026-09-21 14:03"
	if got := EN.Digits(in); got != in {
		t.Errorf("English digits were altered: %q", got)
	}
	got := FA.Digits(in)
	if strings.ContainsAny(got, "0123456789") {
		t.Errorf("Digits left ASCII numerals in %q", got)
	}
	// Separators must survive so a date stays readable.
	if !strings.Contains(got, "-") || !strings.Contains(got, ":") {
		t.Errorf("Digits mangled the separators: %q", got)
	}
}

// The bot's own error values are translation keys, so they must exist.
func TestBotErrorKeysExist(t *testing.T) {
	for _, k := range []string{"bot.expiredconfirm", "bot.notyours", "bot.gone"} {
		if !Has(k) {
			t.Errorf("%s is missing from the table", k)
		}
	}
}
