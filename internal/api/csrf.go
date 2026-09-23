package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

// csrfTTL is how long a token stays valid. The previous window is also
// accepted so a form left open across a boundary still submits.
const csrfTTL = 2 * time.Hour

// CSRF is a stateless token tied to the dashboard user and a time window.
// The dashboard has no session, so there is nothing to key a classic token to.
type CSRF struct {
	secret []byte
	user   string
}

func NewCSRF(secret []byte, user string) *CSRF {
	return &CSRF{secret: secret, user: user}
}

// Token mints the value embedded in every dashboard form.
func (c *CSRF) Token() string { return c.sign(window(time.Now())) }

func (c *CSRF) sign(win int64) string {
	mac := hmac.New(sha256.New, c.secret)
	mac.Write([]byte(c.user + "|" + strconv.FormatInt(win, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

func (c *CSRF) valid(token string) bool {
	if token == "" {
		return false
	}
	now := window(time.Now())
	for _, win := range []int64{now, now - 1} {
		if subtle.ConstantTimeCompare([]byte(token), []byte(c.sign(win))) == 1 {
			return true
		}
	}
	return false
}

func window(t time.Time) int64 { return t.Unix() / int64(csrfTTL.Seconds()) }

// Middleware rejects browser form submissions that carry no valid token.
//
// It deliberately applies only to the three content types an HTML form can
// send cross-origin without a preflight. A JSON POST from another site is
// already stopped by the browser, because the preflight finds no CORS headers
// here — so scripted callers using curl keep working unchanged.
func (c *CSRF) Middleware() fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		switch ctx.Method() {
		case fiber.MethodGet, fiber.MethodHead, fiber.MethodOptions:
			return ctx.Next()
		}
		if !isFormPost(ctx.Get(fiber.HeaderContentType)) {
			return ctx.Next()
		}
		if !c.valid(ctx.FormValue("csrf")) {
			return fiber.NewError(fiber.StatusForbidden,
				"this form has expired — reload the page and try again")
		}
		return ctx.Next()
	}
}

func isFormPost(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i > 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch ct {
	case "application/x-www-form-urlencoded", "multipart/form-data", "text/plain":
		return true
	default:
		return false
	}
}
