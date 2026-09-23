package api

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"
)

// contextWithTimeout derives a cancellable context from a Fiber request.
func contextWithTimeout(c *fiber.Ctx, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.UserContext(), d)
}
