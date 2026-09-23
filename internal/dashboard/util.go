package dashboard

import (
	"context"
	"net/url"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
)

func contextWithTimeout(c *fiber.Ctx, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.UserContext(), d)
}

func urlEscape(s string) string { return url.QueryEscape(s) }

func itoa(n int) string { return strconv.Itoa(n) }
