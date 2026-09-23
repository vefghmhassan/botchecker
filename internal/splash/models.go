// Package splash talks to the upstream configuration API that hands out the
// VLESS server list, and reduces that list to the unique endpoints worth probing.
package splash

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ServerConfig mirrors one entry of the splash API response. The upstream uses
// PascalCase keys, so every field is tagged explicitly.
type ServerConfig struct {
	ID          int       `json:"ID"`
	CreatedAt   time.Time `json:"CreatedAt"`
	UpdatedAt   time.Time `json:"UpdatedAt"`
	Name        string    `json:"Name"`
	Address     string    `json:"Address"`
	Port        int       `json:"Port"`
	Protocol    string    `json:"Protocol"`
	Tags        string    `json:"Tags"`
	Ads         bool      `json:"Ads"`
	CountryCode string    `json:"CountryCode"`
	CountryFlag string    `json:"CountryFlag"`
	IsActive    bool      `json:"IsActive"`
	Capacity    int       `json:"Capacity"`
	RawLink     string    `json:"RawLink"`
}

// Response is the full payload. The two singular fields repeat entries that
// also appear in the lists, so all four sources are merged before probing.
type Response struct {
	NoAds     *ServerConfig  `json:"no_ads"`
	Ads       *ServerConfig  `json:"ads"`
	NoAdsList []ServerConfig `json:"no_ads_list"`
	AdsList   []ServerConfig `json:"ads_list"`
}

// Target is one unique endpoint to probe. Several config entries can point at
// the same Address:Port — in the live data 5.161.158.200:443 arrives twice, as
// config 30378 and 30425 — so the config IDs are collected per target and the
// endpoint is checked once.
type Target struct {
	Address     string
	Port        int
	ConfigIDs   []int
	Names       []string
	CountryCode string
	Protocol    string
	Ads         bool // any config for this endpoint serves ads
	Active      bool // any config for this endpoint is marked active
	// Manual marks an endpoint that is watched on request rather than because
	// splash returned it.
	Manual bool
}

// Key identifies the target, and doubles as the check-host host argument.
func (t Target) Key() string { return fmt.Sprintf("%s:%d", t.Address, t.Port) }

// Configs returns every entry of the response, in a stable order.
func (r *Response) Configs() []ServerConfig {
	var out []ServerConfig
	if r.NoAds != nil {
		out = append(out, *r.NoAds)
	}
	if r.Ads != nil {
		out = append(out, *r.Ads)
	}
	out = append(out, r.NoAdsList...)
	out = append(out, r.AdsList...)
	return out
}

// Targets merges all four sources and deduplicates by Address:Port. Without
// this a single server would be probed once per config entry, burning
// check-host rate limit for no extra information.
func (r *Response) Targets() []Target { return TargetsFrom(r.Configs()) }

// TargetsFrom reduces any list of configs to the endpoints behind them.
//
// Shared by the splash sample and the full node list, so an endpoint is the
// same thing whichever way it was discovered: a production panel holds about
// 4,600 configs on 9 endpoints, and it is the endpoints that get probed.
func TargetsFrom(configs []ServerConfig) []Target {
	byKey := make(map[string]*Target)
	seenID := make(map[string]map[int]bool)
	seenName := make(map[string]map[string]bool)

	for _, cfg := range configs {
		if cfg.Address == "" || cfg.Port <= 0 || cfg.Port > 65535 {
			continue
		}
		key := fmt.Sprintf("%s:%d", cfg.Address, cfg.Port)

		t, ok := byKey[key]
		if !ok {
			t = &Target{
				Address:     cfg.Address,
				Port:        cfg.Port,
				CountryCode: cfg.CountryCode,
				Protocol:    cfg.Protocol,
			}
			byKey[key] = t
			seenID[key] = make(map[int]bool)
			seenName[key] = make(map[string]bool)
		}

		t.Ads = t.Ads || cfg.Ads
		t.Active = t.Active || cfg.IsActive
		if t.CountryCode == "" {
			t.CountryCode = cfg.CountryCode
		}
		if t.Protocol == "" {
			t.Protocol = cfg.Protocol
		}
		if cfg.ID != 0 && !seenID[key][cfg.ID] {
			seenID[key][cfg.ID] = true
			t.ConfigIDs = append(t.ConfigIDs, cfg.ID)
		}
		if cfg.Name != "" && !seenName[key][cfg.Name] {
			seenName[key][cfg.Name] = true
			t.Names = append(t.Names, cfg.Name)
		}
	}

	out := make([]Target, 0, len(byKey))
	for _, t := range byKey {
		sort.Ints(t.ConfigIDs)
		sort.Strings(t.Names)
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Address != out[j].Address {
			return out[i].Address < out[j].Address
		}
		return out[i].Port < out[j].Port
	})
	return out
}

// ParseExtraTargets reads endpoints that are monitored even though the splash
// API does not hand them out.
//
// Splash only returns configs with is_active = true, so a server whose configs
// were switched off disappears from monitoring at exactly the moment it matters
// most — during a replacement's quiet window, or when a machine is still
// carrying users on links they cached earlier.
//
// The format is "address:port" or "address:port:CC", comma separated:
//
//	5.161.128.192:443:US, 5.161.128.192:8443:US
func ParseExtraTargets(s string) []Target {
	var out []Target
	seen := map[string]bool{}

	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		parts := strings.Split(entry, ":")
		if len(parts) < 2 || len(parts) > 3 {
			continue
		}
		address := strings.TrimSpace(parts[0])
		port, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if address == "" || err != nil || port <= 0 || port > 65535 {
			continue
		}

		country := ""
		if len(parts) == 3 {
			country = strings.ToUpper(strings.TrimSpace(parts[2]))
		}

		key := fmt.Sprintf("%s:%d", address, port)
		if seen[key] {
			continue
		}
		seen[key] = true

		out = append(out, Target{
			Address:     address,
			Port:        port,
			CountryCode: country,
			Names:       []string{"manual:" + key},
			// A manually watched endpoint is by definition not one the app is
			// being given, so it is recorded as inactive.
			Active: false,
			Manual: true,
		})
	}
	return out
}

// MergeTargets appends the extra targets the splash response did not contain.
// Splash wins on overlap: it carries the real config ids, names and country.
func MergeTargets(fromSplash, extra []Target) []Target {
	seen := make(map[string]bool, len(fromSplash))
	for _, t := range fromSplash {
		seen[t.Key()] = true
	}
	out := fromSplash
	for _, t := range extra {
		if seen[t.Key()] {
			continue
		}
		seen[t.Key()] = true
		out = append(out, t)
	}
	return out
}

// SummariseNames shows the first few config names and how many there are in
// all.
//
// An endpoint used to arrive with one or two names, because the splash sample
// only ever carried a couple of its configs. Read from the complete node list
// it carries all of them — 1,002 on one production endpoint — and printed whole
// that is a wall of text on the dashboard and roughly 12,000 characters in a
// bot message, which Telegram refuses outright above 4,096.
func SummariseNames(names []string, show int) string {
	if len(names) <= show {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s … +%d (%d configs)",
		strings.Join(names[:show], ", "), len(names)-show, len(names))
}
