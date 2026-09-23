package provision

import (
	"context"
	"net"
	"strconv"
	"time"
)

// bootPoll is how often a new server is knocked on while it boots.
const bootPoll = 3 * time.Second

// bootGrace is given once a port first answers. The listener being up and the
// proxy behind it being ready are a moment apart, and a probe that lands in
// that moment reads a good address as broken.
const bootGrace = 3 * time.Second

// waitForBoot waits until the new server accepts a connection on one of its
// ports, or until the configured boot wait runs out, whichever comes first.
//
// It used to sleep the full boot wait every time. A clone from a snapshot is
// usually listening well before that, and every second spent sleeping past it
// is a second users stay on a dead address — and, when the address turns out
// filtered, a second more before the next location is tried.
//
// The knock is from this machine, not from Iran, so it only answers "has it
// booted", never "is it reachable". That question is still left to the
// check-host verification that follows. Running out of time is not an error
// either: a server that this machine cannot reach may still be fine, and the
// verification decides.
func (m *Manager) waitForBoot(ctx context.Context, address string, ports []int) error {
	limit := m.cfg.ProvisionBootWait
	if limit <= 0 || len(ports) == 0 {
		return nil
	}
	start := time.Now()
	deadline := start.Add(limit)

	for {
		for _, port := range ports {
			if knock(ctx, address, port) {
				waited := time.Since(start).Round(time.Second)
				m.log.Info("the new server is up", "address", address, "port", port, "after", waited)
				m.step(StepBootReady, int(waited.Seconds()))
				return sleep(ctx, bootGrace)
			}
		}
		if time.Now().After(deadline) {
			m.step(StepBootSlow, int(limit.Seconds()))
			return nil
		}
		if err := sleep(ctx, bootPoll); err != nil {
			return err
		}
	}
}

func knock(ctx context.Context, address string, port int) bool {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(address, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
