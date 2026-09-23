package zex

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vefgh/botchecker/internal/settings"

	_ "modernc.org/sqlite"
)

func testSettings(t *testing.T, base, email, password string) *settings.Provider {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value BLOB,
		secret INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}

	p, err := settings.New(db, "", filepath.Join(dir, "k"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		settings.ZexBaseURL: base, settings.ZexAdminEmail: email, settings.ZexAdminPassword: password,
	} {
		if v == "" {
			continue
		}
		if err := p.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

type panelSpy struct {
	mu      sync.Mutex
	logins  int
	bearers []string
	forms   []map[string]string
	// expire makes the next admin call answer as an expired session.
	expire bool
}

func (s *panelSpy) server(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()

	// The real panel answers a good login with a redirect and a Set-Cookie,
	// and a bad one by re-rendering the form with 401.
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		s.logins++
		s.mu.Unlock()

		if r.FormValue("password") != "correct" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("<html>bad credentials</html>"))
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: "admin_token", Value: "jwt-token", Expires: time.Now().Add(12 * time.Hour),
		})
		w.Header().Set("Location", "/admin")
		w.WriteHeader(http.StatusFound)
	})

	admin := func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		expire := s.expire
		s.expire = false
		s.bearers = append(s.bearers, r.Header.Get("Authorization"))
		_ = r.ParseForm()
		form := map[string]string{}
		for k := range r.Form {
			form[k] = r.FormValue(k)
		}
		s.forms = append(s.forms, form)
		s.mu.Unlock()

		if expire {
			// The admin middleware redirects rather than answering 401.
			w.Header().Set("Location", "/login")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"updated":3,"skipped":1,"ids":[1,2,3],"errors":["node 9: bad link"]}`))
	}
	mux.HandleFunc("/admin/v2ray/bulk-active", admin)
	mux.HandleFunc("/admin/v2ray/replace-address", admin)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (s *panelSpy) loginCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins
}

func TestLoginCapturesTheSessionCookie(t *testing.T) {
	spy := &panelSpy{}
	srv := spy.server(t)
	c := New(testSettings(t, srv.URL, "admin@example.com", "correct"), 5*time.Second)

	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if c.token != "jwt-token" {
		t.Errorf("token = %q, want the cookie value", c.token)
	}
}

func TestLoginRejectsBadCredentials(t *testing.T) {
	spy := &panelSpy{}
	srv := spy.server(t)
	c := New(testSettings(t, srv.URL, "admin@example.com", "wrong"), 5*time.Second)

	err := c.Login(context.Background())
	if err == nil {
		t.Fatal("Login succeeded with the wrong password")
	}
	// The password must not travel into an error that gets logged or stored.
	if contains(err.Error(), "wrong") {
		t.Errorf("the error leaks the password: %v", err)
	}
}

// The session is reused: a burst of calls must not log in every time.
func TestSessionIsCached(t *testing.T) {
	spy := &panelSpy{}
	srv := spy.server(t)
	c := New(testSettings(t, srv.URL, "admin@example.com", "correct"), 5*time.Second)

	for i := 0; i < 3; i++ {
		if _, err := c.BulkActive(context.Background(), "1.2.3.4", false); err != nil {
			t.Fatalf("BulkActive: %v", err)
		}
	}
	if got := spy.loginCount(); got != 1 {
		t.Errorf("logged in %d times, want 1", got)
	}
}

// The panel redirects an expired session to /login instead of answering 401,
// so that has to be recognised and retried once.
func TestExpiredSessionLogsInAgain(t *testing.T) {
	spy := &panelSpy{}
	srv := spy.server(t)
	c := New(testSettings(t, srv.URL, "admin@example.com", "correct"), 5*time.Second)

	if _, err := c.BulkActive(context.Background(), "1.2.3.4", false); err != nil {
		t.Fatal(err)
	}
	spy.mu.Lock()
	spy.expire = true
	spy.mu.Unlock()

	if _, err := c.BulkActive(context.Background(), "1.2.3.4", false); err != nil {
		t.Fatalf("the call did not recover from an expired session: %v", err)
	}
	if got := spy.loginCount(); got != 2 {
		t.Errorf("logged in %d times, want a second login after the redirect", got)
	}
}

func TestBulkActiveSendsTheRightForm(t *testing.T) {
	spy := &panelSpy{}
	srv := spy.server(t)
	c := New(testSettings(t, srv.URL, "admin@example.com", "correct"), 5*time.Second)

	n, err := c.BulkActive(context.Background(), "5.161.158.200", false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("updated = %d, want 3", n)
	}

	spy.mu.Lock()
	form := spy.forms[0]
	bearer := spy.bearers[0]
	spy.mu.Unlock()

	if form["address"] != "5.161.158.200" || form["active"] != "false" {
		t.Errorf("form = %v, want the address and active=false", form)
	}
	if bearer != "Bearer jwt-token" {
		t.Errorf("Authorization = %q, want the session token", bearer)
	}
}

func TestReplaceAddressCarriesEveryField(t *testing.T) {
	spy := &panelSpy{}
	srv := spy.server(t)
	c := New(testSettings(t, srv.URL, "admin@example.com", "correct"), 5*time.Second)

	res, err := c.ReplaceAddress(context.Background(), ReplaceRequest{
		OldAddress: "5.161.158.200", NewAddress: "88.99.1.2",
		Country: "DE", Activate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated != 3 || res.Skipped != 1 || len(res.Errors) != 1 {
		t.Errorf("result = %+v, want the panel's counts carried through", res)
	}

	spy.mu.Lock()
	form := spy.forms[0]
	spy.mu.Unlock()

	for key, want := range map[string]string{
		"old_address": "5.161.158.200", "new_address": "88.99.1.2",
		"country": "DE", "activate": "true",
	} {
		if form[key] != want {
			t.Errorf("form %s = %q, want %q", key, form[key], want)
		}
	}
}

// The panel applies new_port to every config on the address, whatever port that
// config was actually on. Sending it moved 200 live configs off 8443 and onto
// 443 with nothing checking that 443 worked for them. Nothing may send it again.
func TestReplaceAddressNeverSendsAPort(t *testing.T) {
	spy := &panelSpy{}
	srv := spy.server(t)
	c := New(testSettings(t, srv.URL, "admin@example.com", "correct"), 5*time.Second)

	if _, err := c.ReplaceAddress(context.Background(), ReplaceRequest{
		OldAddress: "5.161.158.200", NewAddress: "88.99.1.2",
		Country: "DE", Activate: true,
	}); err != nil {
		t.Fatal(err)
	}

	spy.mu.Lock()
	form := spy.forms[0]
	spy.mu.Unlock()

	if v, ok := form["new_port"]; ok {
		t.Fatalf("new_port was sent as %q — every config on the address would be forced onto it", v)
	}
}

func TestNotConfigured(t *testing.T) {
	c := New(testSettings(t, "", "", ""), time.Second)

	if c.Configured() {
		t.Error("Configured() = true with nothing set")
	}
	if _, err := c.BulkActive(context.Background(), "1.2.3.4", false); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
