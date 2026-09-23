// Package api exposes the JSON endpoints.
package api

import (
	"crypto/subtle"
	"encoding/base64"
	"strings"

	"github.com/gofiber/fiber/v2"
)

// BasicAuth guards the dashboard and the JSON API. The health endpoint is
// left open so container health checks keep working.
func BasicAuth(user, pass string) fiber.Handler {
	expectedUser := []byte(user)
	expectedPass := []byte(pass)

	return func(c *fiber.Ctx) error {
		if c.Path() == "/healthz" {
			return c.Next()
		}

		header := c.Get(fiber.HeaderAuthorization)
		const prefix = "Basic "
		if !strings.HasPrefix(header, prefix) {
			return challenge(c)
		}

		raw, err := base64.StdEncoding.DecodeString(header[len(prefix):])
		if err != nil {
			return challenge(c)
		}
		gotUser, gotPass, ok := strings.Cut(string(raw), ":")
		if !ok {
			return challenge(c)
		}

		// Constant-time comparison on both halves so neither can be probed
		// character by character.
		userOK := subtle.ConstantTimeCompare([]byte(gotUser), expectedUser) == 1
		passOK := subtle.ConstantTimeCompare([]byte(gotPass), expectedPass) == 1
		if !userOK || !passOK {
			return challenge(c)
		}
		return c.Next()
	}
}

func challenge(c *fiber.Ctx) error {
	c.Set(fiber.HeaderWWWAuthenticate, `Basic realm="botchecker", charset="UTF-8"`)
	return c.SendStatus(fiber.StatusUnauthorized)
}
