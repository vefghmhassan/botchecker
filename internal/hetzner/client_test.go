package hetzner

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient points a client at a stub API with a working token.
func newTestClient(t *testing.T, baseURL, token string) *Client {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(testSettings(t, token), log, 5*time.Second, time.Minute).WithBaseURL(baseURL)
}

// Hetzner answers 403 for a full project as well as a bad token, so the
// message it sends has to survive — "rejected the API token" sends the reader
// looking at permissions when the real problem is the server limit.
func TestForbiddenReportsWhatHetznerSaid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"resource_limit_exceeded","message":"server limit reached"}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, "tok")
	_, err := c.CreateFromSnapshot(context.Background(), CreateSpec{
		Name: "x", ServerType: "cpx32", Image: "1", Location: "hel1",
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "server limit reached") {
		t.Errorf("error hides the cause: %v", err)
	}
	if strings.Contains(err.Error(), "rejected the API token") {
		t.Errorf("a full project was reported as an auth failure: %v", err)
	}
}

// A 403 with no usable body still has to say something.
func TestForbiddenWithoutAMessageStillMentionsTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, "tok")
	if _, err := c.Ping(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "token") {
		t.Errorf("got %v, want the token mentioned", err)
	}
}
