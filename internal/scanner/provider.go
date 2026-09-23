package scanner

import (
	"context"

	"github.com/vefgh/botchecker/internal/splash"
)

// ProviderInfo says which provider an address belongs to.
type ProviderInfo struct {
	Provider string // hetzner | external | unknown
	// Project names which Hetzner account holds it. Server ids are per project,
	// so an id without this cannot safely be acted on.
	Project      string
	Kind         string // server | primary_ip | floating_ip
	ServerID     int64
	ServerName   string
	AbuseBlocked bool
}

// ProviderResolver classifies an address against a cloud account. The scanner
// takes it as an interface so it does not depend on any particular provider.
type ProviderResolver interface {
	Resolve(ctx context.Context, address string) ProviderInfo
}

// SetProviderResolver enables ownership tagging during full scans. Passing nil
// disables it, and every endpoint stays "unknown".
func (s *Scanner) SetProviderResolver(r ProviderResolver) {
	s.mu.Lock()
	s.provider = r
	s.mu.Unlock()
}

func (s *Scanner) resolver() ProviderResolver {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.provider
}

// ExtraTargetSource supplies endpoints to probe that the splash API does not
// return. It is read on every scan so the list can change without a restart.
type ExtraTargetSource func() []splash.Target

// SetExtraTargets adds endpoints that are monitored even though the app is not
// being given them.
//
// Splash only returns configs with is_active = true, so a server whose configs
// are switched off vanishes from monitoring — including during a replacement's
// own quiet window. Passing nil turns this off.
func (s *Scanner) SetExtraTargets(f ExtraTargetSource) {
	s.mu.Lock()
	s.extra = f
	s.mu.Unlock()
}

func (s *Scanner) extraTargets() []splash.Target {
	s.mu.Lock()
	f := s.extra
	s.mu.Unlock()
	if f == nil {
		return nil
	}
	return f()
}

// TargetSource produces the endpoints a full scan probes, and how many configs
// they came from.
//
// The inventory sync provides it. Without one the scanner falls back to the
// app's splash endpoint, which returns a random handful of configs rather than
// the fleet — correct for tests and for an install that has not been wired up,
// wrong for monitoring.
type TargetSource func(ctx context.Context) ([]splash.Target, int, error)

// SetTargetSource installs the inventory as the source of endpoints.
func (s *Scanner) SetTargetSource(f TargetSource) {
	s.mu.Lock()
	s.source = f
	s.mu.Unlock()
}

// discover asks the target source, or falls back to the splash sample.
func (s *Scanner) discover(ctx context.Context) ([]splash.Target, int, error) {
	s.mu.Lock()
	src := s.source
	s.mu.Unlock()
	if src != nil {
		return src(ctx)
	}

	resp, err := s.splash.Fetch(ctx)
	if err != nil {
		return nil, 0, err
	}
	return splash.MergeTargets(resp.Targets(), s.extraTargets()), len(resp.Configs()), nil
}

// StartTargets runs a scan over a given list of endpoints instead of the whole
// fleet. The update button uses it: after a sync finds ten new servers, those
// ten are probed straight away rather than waiting for the next full scan.
//
// The list travels with the run rather than being parked on the scanner. Parked,
// a scheduled scan starting in the gap could pick it up and probe only those
// ten, skipping the rest of the fleet for a whole interval.
func (s *Scanner) StartTargets(ctx context.Context, trigger string, targets []splash.Target) (int64, error) {
	if len(targets) == 0 {
		return 0, nil
	}
	return s.start(ctx, trigger, append([]splash.Target(nil), targets...))
}
