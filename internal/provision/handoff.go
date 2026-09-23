package provision

import (
	"context"
	"log/slog"
	"time"
)

// ProvisionedServer is a server that has been created and confirmed reachable
// from Iran. It carries everything a consumer needs to put the new address
// into service, including the config rows that still point at the old one.
type ProvisionedServer struct {
	// ReplacesAddress is the machine that was blocked.
	ReplacesAddress string
	// Ports are every port that machine served, and that the replacement was
	// verified on. There is no single port: a blocked machine takes all of its
	// ports down together and the replacement carries all of them across.
	Ports []int
	// ConfigIDs are the splash config rows pointing at the blocked address.
	ConfigIDs []int
	// Names are the config names, for logging and messages.
	Names []string

	// Address is the new server's public IPv4.
	Address string

	HetznerServerID int64
	SnapshotID      string
	VerifiedAt      time.Time
}

// Handoff receives a verified replacement server.
//
// This is the extension point where the new address gets wired into whatever
// consumes it — registering it as a node in the 3x-ui panel, updating the
// splash config rows, or handing it to another service.
//
// TODO: intentionally left unimplemented. The owner wants to test the
// downstream project separately before this runs automatically. Until then
// NoopHandoff records the server and the alert reports the new address so the
// step can be done by hand.
//
// When it is implemented, the pieces already exist:
//   - internal/xui has NodeTest / NodeAdd / NodeDelete against
//     POST /panel/api/nodes/test, /add and /del/{id}
//   - ProvisionedServer.ConfigIDs identifies the splash rows to update
//   - note that 3x-ui does not migrate inbounds when a node is deleted, so the
//     old node must be drained before it is removed
type Handoff interface {
	OnServerReady(ctx context.Context, s ProvisionedServer) error
}

// NoopHandoff is the default: it records the server and does nothing else.
type NoopHandoff struct{ log *slog.Logger }

func NewNoopHandoff(log *slog.Logger) *NoopHandoff { return &NoopHandoff{log: log} }

func (h *NoopHandoff) OnServerReady(_ context.Context, s ProvisionedServer) error {
	h.log.Info("replacement server is ready and awaiting manual handoff",
		"new_address", s.Address,
		"replaces", s.ReplacesAddress,
		"config_ids", s.ConfigIDs,
		"hetzner_server_id", s.HetznerServerID)
	return nil
}
