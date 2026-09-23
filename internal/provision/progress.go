package provision

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vefgh/botchecker/internal/store"
)

// Progress is what the run in flight has done so far, step by step, so a
// person who pressed the button can watch it rather than wait for alerts.
//
// Steps carry a translation key and its arguments instead of prose: this
// package does not know which language the reader has chosen, and the bot
// does.
type Progress struct {
	Running    bool
	Address    string
	Started    time.Time
	Finished   time.Time
	Steps      []Step
	NewAddress string
	// Err is the reason the run failed, empty when it did not.
	Err string
}

// Step is one line of the progress log.
type Step struct {
	At   time.Time
	Key  string
	Args []any
}

// The step keys. Each is a translation key in internal/i18n, and its
// arguments must match the verbs there.
const (
	StepStart        = "prog.start"        // address
	StepSkipped      = "prog.skipped"      // reason
	StepDryRun       = "prog.dryrun"       //
	StepSnapshotAuto = "prog.snapshotauto" // project, id, created, configured
	StepTryLocation  = "prog.trylocation"  // location, country, project, snapshot
	StepReserved     = "prog.reserved"     // address
	StepCreated      = "prog.created"      // server id, address, type
	StepCreateFailed = "prog.createfailed" // location, reason
	StepBootReady    = "prog.bootready"    // seconds
	StepBootSlow     = "prog.bootslow"     // seconds
	StepTesting      = "prog.testing"      // address, ports
	StepHealthy      = "prog.healthy"      // address, per-port
	StepUnhealthy    = "prog.unhealthy"    // address, verdict, per-port
	StepDiscarded    = "prog.discarded"    // address
	StepFloatTry     = "prog.floattry"     // location
	StepFloatSkip    = "prog.floatskip"    // reason
	StepFloatReady   = "prog.floatready"   // address
	StepMoving       = "prog.moving"       // configs, old, new
	StepMoved        = "prog.moved"        // updated, skipped
	StepRetired      = "prog.retired"      // old address, server id
	StepConfigsOff   = "prog.configsoff"   // configs
)

// maxSteps bounds the log so a run that loops through many locations cannot
// grow a Telegram message past its 4,096 character limit.
const maxSteps = 40

type progressLog struct {
	mu sync.Mutex
	p  Progress
}

// Progress returns a copy of the current or most recent run.
func (m *Manager) Progress() Progress {
	m.prog.mu.Lock()
	defer m.prog.mu.Unlock()
	out := m.prog.p
	out.Steps = append([]Step(nil), m.prog.p.Steps...)
	return out
}

func (m *Manager) beginProgress(address string) {
	m.prog.mu.Lock()
	m.prog.p = Progress{Running: true, Address: address, Started: time.Now()}
	m.prog.mu.Unlock()
	m.step(StepStart, address)
}

func (m *Manager) step(key string, args ...any) {
	m.prog.mu.Lock()
	defer m.prog.mu.Unlock()
	if !m.prog.p.Running {
		return
	}
	steps := append(m.prog.p.Steps, Step{At: time.Now(), Key: key, Args: args})
	if len(steps) > maxSteps {
		// The first line says what the run is about, so it is kept; what goes
		// is the oldest detail after it.
		steps = append(steps[:1], steps[len(steps)-maxSteps+1:]...)
	}
	m.prog.p.Steps = steps
}

func (m *Manager) progressNewAddress(address string) {
	m.prog.mu.Lock()
	m.prog.p.NewAddress = address
	m.prog.mu.Unlock()
}

func (m *Manager) finishProgress(err error) {
	m.prog.mu.Lock()
	defer m.prog.mu.Unlock()
	m.prog.p.Running = false
	m.prog.p.Finished = time.Now()
	if err != nil {
		m.prog.p.Err = err.Error()
	}
}

// TriggerAsync starts a replacement in the background and returns as soon as
// it holds the run, so the caller can follow Progress from the first step.
//
// Starting it with a bare goroutine would race: a follower reading Progress
// before the goroutine got going would see the previous, finished run and stop
// at once.
func (m *Manager) TriggerAsync(ctx context.Context, addr store.AddressStatus, outageCount int, ov Override) error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return ErrBusy
	}
	m.running = true
	m.mu.Unlock()
	m.beginProgress(addr.Address)

	go func() {
		err := m.runHeld(context.WithoutCancel(ctx), addr, outageCount, ov)
		if err != nil {
			m.log.Warn("manual replacement failed", "address", addr.Address, "err", err)
		}
	}()
	return nil
}

// runHeld runs with the lock already taken, and gives it back.
func (m *Manager) runHeld(ctx context.Context, addr store.AddressStatus, outageCount int, ov Override) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("replacement crashed: %v", r)
			m.log.Error("replacement crashed", "address", addr.Address, "panic", r)
		}
		m.finishProgress(err)
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
	}()
	return m.run(ctx, addr, outageCount, ov)
}
