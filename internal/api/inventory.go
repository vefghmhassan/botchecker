package api

import (
	"context"
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/vefgh/botchecker/internal/inventory"
)

// updateInventory re-reads the panel now and probes whatever is new.
//
// Runs against the request's own deadline rather than in the background: the
// caller is a person who pressed a button and is waiting to see what changed,
// and the read is one request.
func (h *Handler) updateInventory(c *fiber.Ctx) error {
	if h.d.Inventory == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "the inventory is not configured")
	}
	// Detached from the request so the probe it starts is not cancelled the
	// moment the response is written; bounded so a hung panel cannot hold the
	// handler forever.
	ctx, cancel := context.WithTimeout(h.bg, 60*time.Second)
	defer cancel()

	res, scanID, err := h.d.Inventory.Update(ctx)
	busy := errors.Is(err, inventory.ErrProberBusy)
	if err != nil && !busy {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": err.Error(), "result": res})
	}

	out := fiber.Map{"result": res}
	if scanID != 0 {
		out["probing_scan_id"] = scanID
	}
	if busy {
		out["note"] = err.Error()
	}
	return c.JSON(out)
}

// inventoryState is the last sync, for a caller that only wants to know how
// fresh the list is.
func (h *Handler) inventoryState(c *fiber.Ctx) error {
	if h.d.Inventory == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "the inventory is not configured")
	}
	return c.JSON(h.d.Inventory.Last())
}
