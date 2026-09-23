package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
)

func csrfApp(t *testing.T) (*fiber.App, *CSRF) {
	t.Helper()
	c := NewCSRF([]byte("test-secret"), "admin")

	app := fiber.New()
	app.Use(c.Middleware())
	app.Post("/settings", func(ctx *fiber.Ctx) error { return ctx.SendString("saved") })
	app.Post("/api/v1/scan", func(ctx *fiber.Ctx) error { return ctx.SendString("started") })
	app.Get("/", func(ctx *fiber.Ctx) error { return ctx.SendString("ok") })
	return app, c
}

func post(t *testing.T, app *fiber.App, path, contentType, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp.StatusCode
}

func TestFormPostRequiresToken(t *testing.T) {
	app, c := csrfApp(t)

	if got := post(t, app, "/settings", fiber.MIMEApplicationForm, "group=telegram"); got != http.StatusForbidden {
		t.Errorf("form post without a token = %d, want 403", got)
	}

	body := "csrf=" + c.Token() + "&group=telegram"
	if got := post(t, app, "/settings", fiber.MIMEApplicationForm, body); got != http.StatusOK {
		t.Errorf("form post with a valid token = %d, want 200", got)
	}
}

func TestPreviousWindowIsAccepted(t *testing.T) {
	c := NewCSRF([]byte("test-secret"), "admin")

	// A form rendered just before a window boundary must still submit.
	previous := c.sign(window(time.Now()) - 1)
	if !c.valid(previous) {
		t.Error("a token from the previous window was rejected")
	}
	if c.valid(c.sign(window(time.Now()) - 5)) {
		t.Error("a long-expired token was accepted")
	}
}

func TestTokenIsTiedToTheUser(t *testing.T) {
	a := NewCSRF([]byte("test-secret"), "admin")
	b := NewCSRF([]byte("test-secret"), "someone-else")

	if a.valid(b.Token()) {
		t.Error("a token minted for another user was accepted")
	}
}

// The API is meant to stay scriptable. A JSON body cannot be sent
// cross-origin by a plain HTML form without a preflight, so it needs no token.
func TestJSONPostIsExempt(t *testing.T) {
	app, _ := csrfApp(t)

	if got := post(t, app, "/api/v1/scan", fiber.MIMEApplicationJSON, `{"x":1}`); got != http.StatusOK {
		t.Errorf("json post = %d, want 200", got)
	}
}

// curl -X POST with no body sends no Content-Type at all; blocking that would
// break the documented way to start a scan.
func TestBodylessPostIsExempt(t *testing.T) {
	app, _ := csrfApp(t)

	if got := post(t, app, "/api/v1/scan", "", ""); got != http.StatusOK {
		t.Errorf("bodyless post = %d, want 200", got)
	}
}

func TestGetIsNeverBlocked(t *testing.T) {
	app, _ := csrfApp(t)

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil), 5000)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET = %d, want 200", resp.StatusCode)
	}
}

func TestMultipartIsGuarded(t *testing.T) {
	app, _ := csrfApp(t)

	got := post(t, app, "/settings", "multipart/form-data; boundary=xyz", "")
	if got != http.StatusForbidden {
		t.Errorf("multipart post without a token = %d, want 403", got)
	}
}
