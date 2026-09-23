package provision

import (
	"sort"
	"strings"
	"time"

	"github.com/vefgh/botchecker/internal/scanner"
	"github.com/vefgh/botchecker/internal/store"
)

// CountryHealth summarises how one country's addresses behave from Iran.
type CountryHealth struct {
	Country string
	// Healthy counts addresses where every Iranian probe connected.
	Healthy int
	// Total is how many addresses that country has.
	Total int
	// LastChecked is the most recent probe across the country's addresses.
	LastChecked time.Time
	// Locations are the Hetzner locations that sit in this country.
	Locations []string
}

// Eligible reports whether a replacement may be created here: every address in
// the country must be fully healthy from Iran, and there must be somewhere to
// create it.
func (c CountryHealth) Eligible() bool {
	return c.Total > 0 && c.Healthy == c.Total && len(c.Locations) > 0
}

// RankCountries orders the countries a replacement could be created in, best
// first.
//
// A country only qualifies when *every* one of its addresses answers from all
// eight Iranian probes. One filtered address is enough to disqualify it: buying
// into a range that is already being blocked would waste the server.
//
// The country the failing address is in is excluded outright — it is the one
// place we know is not working.
func RankCountries(targets []store.TargetStatus, exclude string, locations map[string][]string) []CountryHealth {
	exclude = strings.ToUpper(strings.TrimSpace(exclude))

	var out []CountryHealth
	for _, c := range SurveyCountries(targets, locations) {
		if c.Country == exclude || !c.Eligible() {
			continue
		}
		out = append(out, c)
	}
	return out
}

// SurveyCountries reports every country the fleet touches, best first, whether
// or not it qualifies.
//
// RankCountries answers "where may we build?" and drops everything else, which
// makes a refusal impossible to explain. This answers "how does each country
// look?", so the dashboard and the bot can show why nowhere was eligible.
func SurveyCountries(targets []store.TargetStatus, locations map[string][]string) []CountryHealth {
	byCountry := map[string]*CountryHealth{}

	for _, t := range targets {
		country := strings.ToUpper(strings.TrimSpace(t.CountryCode))
		if country == "" {
			continue
		}

		c, ok := byCountry[country]
		if !ok {
			c = &CountryHealth{Country: country, Locations: locations[country]}
			byCountry[country] = c
		}
		c.Total++
		if t.Verdict == string(scanner.VerdictHealthy) && t.IRTotal > 0 && t.IROpen == t.IRTotal {
			c.Healthy++
		}
		if t.CheckedAt != nil && t.CheckedAt.After(c.LastChecked) {
			c.LastChecked = *t.CheckedAt
		}
	}

	out := make([]CountryHealth, 0, len(byCountry))
	for _, c := range byCountry {
		out = append(out, *c)
	}

	sort.Slice(out, func(i, j int) bool {
		// Eligible countries first, so the one that would be chosen leads.
		if out[i].Eligible() != out[j].Eligible() {
			return out[i].Eligible()
		}
		// More proven-healthy addresses first: a country with five working
		// servers is better evidence than one with a single server.
		if out[i].Healthy != out[j].Healthy {
			return out[i].Healthy > out[j].Healthy
		}
		if !out[i].LastChecked.Equal(out[j].LastChecked) {
			return out[i].LastChecked.After(out[j].LastChecked)
		}
		return out[i].Country < out[j].Country
	})
	return out
}

// ParseLocationMap reads "FI:hel1,DE:fsn1,DE:nbg1" into country → locations,
// preserving the order each country's locations were listed in.
func ParseLocationMap(s string) map[string][]string {
	out := map[string][]string{}
	for _, pair := range strings.Split(s, ",") {
		country, location, ok := strings.Cut(strings.TrimSpace(pair), ":")
		if !ok {
			continue
		}
		country = strings.ToUpper(strings.TrimSpace(country))
		location = strings.TrimSpace(location)
		if country == "" || location == "" {
			continue
		}
		out[country] = append(out[country], location)
	}
	return out
}
