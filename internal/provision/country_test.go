package provision

import (
	"testing"
	"time"

	"github.com/vefgh/botchecker/internal/store"
)

func target(address, country, verdict string, irOpen, irTotal int, checked time.Time) store.TargetStatus {
	return store.TargetStatus{
		Address: address, Port: 443, CountryCode: country, Verdict: verdict,
		IROpen: irOpen, IRTotal: irTotal, CheckedAt: &checked,
	}
}

var testLocations = map[string][]string{
	"FI": {"hel1"}, "DE": {"fsn1", "nbg1"}, "US": {"ash", "hil"}, "NL": {"ams"},
}

// A country is only worth buying into when every address there works. One
// filtered address means the range is already being blocked.
func TestOneUnhealthyAddressDisqualifiesACountry(t *testing.T) {
	now := time.Now()
	targets := []store.TargetStatus{
		target("1.1.1.1", "FI", "HEALTHY", 8, 8, now),
		target("1.1.1.2", "FI", "BLOCKED_IR", 0, 8, now),
		target("2.2.2.1", "DE", "HEALTHY", 8, 8, now),
		target("2.2.2.2", "DE", "HEALTHY", 8, 8, now),
	}

	got := RankCountries(targets, "US", testLocations)
	if len(got) != 1 {
		t.Fatalf("ranked %d countries, want only DE: %+v", len(got), got)
	}
	if got[0].Country != "DE" {
		t.Errorf("first choice = %s, want DE", got[0].Country)
	}
}

// Partially reachable is not healthy: 7 of 8 probes means some carriers
// already cannot get there.
func TestPartialReachabilityIsNotHealthy(t *testing.T) {
	now := time.Now()
	targets := []store.TargetStatus{target("1.1.1.1", "FI", "HEALTHY", 7, 8, now)}

	if got := RankCountries(targets, "", testLocations); len(got) != 0 {
		t.Fatalf("ranked %+v, want nothing — 7/8 is not fully healthy", got)
	}
}

func TestExcludedCountryIsNeverChosen(t *testing.T) {
	now := time.Now()
	targets := []store.TargetStatus{
		target("1.1.1.1", "FI", "HEALTHY", 8, 8, now),
		target("2.2.2.2", "DE", "HEALTHY", 8, 8, now),
	}

	got := RankCountries(targets, "fi", testLocations) // case-insensitive
	for _, c := range got {
		if c.Country == "FI" {
			t.Fatal("the failing address's own country was offered as a replacement")
		}
	}
	if len(got) != 1 || got[0].Country != "DE" {
		t.Fatalf("ranked %+v, want only DE", got)
	}
}

// Without a location there is nowhere to create the server, however healthy
// the country looks.
func TestCountryWithoutALocationIsSkipped(t *testing.T) {
	now := time.Now()
	targets := []store.TargetStatus{target("9.9.9.9", "SE", "HEALTHY", 8, 8, now)}

	if got := RankCountries(targets, "", testLocations); len(got) != 0 {
		t.Fatalf("ranked %+v, want nothing — SE has no Hetzner location", got)
	}
}

func TestMoreHealthyAddressesRankHigher(t *testing.T) {
	now := time.Now()
	targets := []store.TargetStatus{
		target("1.1.1.1", "FI", "HEALTHY", 8, 8, now),
		target("2.2.2.1", "DE", "HEALTHY", 8, 8, now),
		target("2.2.2.2", "DE", "HEALTHY", 8, 8, now),
		target("2.2.2.3", "DE", "HEALTHY", 8, 8, now),
	}

	got := RankCountries(targets, "", testLocations)
	if len(got) != 2 || got[0].Country != "DE" {
		t.Fatalf("ranked %+v, want DE first on three healthy addresses", got)
	}
	if got[0].Locations[0] != "fsn1" {
		t.Errorf("locations = %v, want the configured order preserved", got[0].Locations)
	}
}

func TestNoEligibleCountryIsACleanEmptyResult(t *testing.T) {
	now := time.Now()
	targets := []store.TargetStatus{
		target("1.1.1.1", "FI", "BLOCKED_IR", 0, 8, now),
		target("2.2.2.2", "DE", "SERVER_DOWN", 0, 8, now),
	}

	if got := RankCountries(targets, "", testLocations); len(got) != 0 {
		t.Fatalf("ranked %+v, want nothing when everything is broken", got)
	}
}

func TestParseLocationMap(t *testing.T) {
	got := ParseLocationMap("FI:hel1, DE:fsn1 , DE:nbg1,US:ash,,bad,:x,Y:")

	if len(got["DE"]) != 2 || got["DE"][0] != "fsn1" || got["DE"][1] != "nbg1" {
		t.Errorf("DE = %v, want both locations in order", got["DE"])
	}
	if len(got["FI"]) != 1 || got["FI"][0] != "hel1" {
		t.Errorf("FI = %v", got["FI"])
	}
	if len(got) != 3 {
		t.Errorf("parsed %d countries (%v), want the malformed entries dropped", len(got), got)
	}
}

// The survey exists to explain a refusal, so it must keep the countries that
// RankCountries throws away.
func TestSurveyKeepsTheCountriesThatCannotHost(t *testing.T) {
	now := time.Now()
	targets := []store.TargetStatus{
		target("1.1.1.1", "FI", "HEALTHY", 8, 8, now),
		target("2.2.2.2", "US", "BLOCKED_IR", 0, 8, now),
		target("3.3.3.3", "US", "HEALTHY", 8, 8, now),
		// SE has no Hetzner location mapped, so it is healthy but unusable.
		target("4.4.4.4", "SE", "HEALTHY", 8, 8, now),
	}

	survey := SurveyCountries(targets, testLocations)
	if len(survey) != 3 {
		t.Fatalf("survey has %d countries, want 3: %+v", len(survey), survey)
	}

	byCountry := map[string]CountryHealth{}
	for _, c := range survey {
		byCountry[c.Country] = c
	}
	if us := byCountry["US"]; us.Healthy != 1 || us.Total != 2 || us.Eligible() {
		t.Fatalf("US = %+v, want 1/2 and ineligible", us)
	}
	if se := byCountry["SE"]; !(se.Healthy == 1 && se.Total == 1) || se.Eligible() {
		t.Fatalf("SE = %+v, want healthy but ineligible for lack of a location", se)
	}
	if fi := byCountry["FI"]; !fi.Eligible() {
		t.Fatalf("FI = %+v, want eligible", fi)
	}
	// The one that would actually be chosen has to lead.
	if survey[0].Country != "FI" {
		t.Fatalf("survey leads with %q, want the eligible country first", survey[0].Country)
	}
}

// The survey is a superset of the ranking, minus the excluded country.
func TestSurveyAgreesWithRanking(t *testing.T) {
	now := time.Now()
	targets := []store.TargetStatus{
		target("1.1.1.1", "FI", "HEALTHY", 8, 8, now),
		target("2.2.2.2", "DE", "HEALTHY", 8, 8, now),
		target("3.3.3.3", "US", "PARTIAL_BLOCK", 1, 8, now),
	}

	ranked := RankCountries(targets, "DE", testLocations)
	if len(ranked) != 1 || ranked[0].Country != "FI" {
		t.Fatalf("ranked = %+v, want FI alone", ranked)
	}

	var eligible []string
	for _, c := range SurveyCountries(targets, testLocations) {
		if c.Eligible() {
			eligible = append(eligible, c.Country)
		}
	}
	if len(eligible) != 2 {
		t.Fatalf("survey says %v are eligible, want FI and DE", eligible)
	}
}
