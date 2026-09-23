package hetzner

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vefgh/botchecker/internal/settings"
)

// projectAPI stands in for one Hetzner account. It asserts its own bearer, so a
// request that reached the wrong project's stub fails loudly rather than
// quietly returning someone else's servers.
func projectAPI(t *testing.T, token string, addresses ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("request carried %q, want the token of this project (%q)", got, token)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/servers":
			servers := make([]any, 0, len(addresses))
			for i, a := range addresses {
				servers = append(servers, map[string]any{
					"id": 100 + i, "name": "srv", "status": "running",
					"public_net":  map[string]any{"ipv4": map[string]any{"ip": a}},
					"server_type": map[string]any{"name": "cpx31"},
					"datacenter":  map[string]any{"location": map[string]any{"name": "ash"}},
				})
			}
			writeJSON(w, map[string]any{"servers": servers, "meta": map[string]any{}})
		default:
			writeJSON(w, map[string]any{"primary_ips": []any{}, "floating_ips": []any{}, "meta": map[string]any{}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func registryWith(t *testing.T, tokens, projects string) *Registry {
	t.Helper()
	set := testSettings(t, "")
	if err := set.Set(settings.HetznerTokens, tokens); err != nil {
		t.Fatal(err)
	}
	if err := set.Set(settings.HetznerProjects, projects); err != nil {
		t.Fatal(err)
	}
	return NewRegistry(set, slog.New(slog.NewTextHandler(io.Discard, nil)), 5*time.Second, time.Minute)
}

func TestEachProjectSendsItsOwnToken(t *testing.T) {
	a := projectAPI(t, "token-a", "1.1.1.1")

	reg := registryWith(t, "main=token-a, spare=token-b", "")
	reg.WithBaseURL(a.URL)

	// Only main's client may talk to main's API; spare's bearer would fail the
	// assertion inside the stub.
	cli, ok := reg.Client("main")
	if !ok {
		t.Fatal("no client for main")
	}
	if _, err := cli.Inventory(context.Background()); err != nil {
		t.Fatalf("inventory: %v", err)
	}
}

func TestLocateNamesTheProjectHoldingTheAddress(t *testing.T) {
	set := testSettings(t, "")
	_ = set.Set(settings.HetznerTokens, "main=token-a")
	_ = set.Set(settings.HetznerProjects, "main|snapshot=1|type=cpx31|locations=ash")

	api := projectAPI(t, "token-a", "5.161.158.200")
	reg := NewRegistry(set, slog.New(slog.NewTextHandler(io.Discard, nil)), 5*time.Second, time.Minute).
		WithBaseURL(api.URL)

	got, err := reg.Locate(context.Background(), "5.161.158.200")
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	if got.Project != "main" || got.Ownership != OwnedHetzner {
		t.Fatalf("got %+v, want main/hetzner", got)
	}

	// An address nobody holds is external, and that is safe to say because
	// every project answered.
	missing, err := reg.Locate(context.Background(), "203.0.113.9")
	if err != nil {
		t.Fatalf("locate missing: %v", err)
	}
	if missing.Ownership != OwnedExternal {
		t.Errorf("unknown address = %s, want external", missing.Ownership)
	}
}

// A project that could not be read makes the whole view partial. Reporting
// "external" from a partial view is a claim the data does not support, and
// external is the label that stops an address ever being replaced.
func TestLocateSaysUnknownWhenAProjectCannotBeRead(t *testing.T) {
	set := testSettings(t, "")
	_ = set.Set(settings.HetznerTokens, "main=token-a")

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(dead.Close)

	reg := NewRegistry(set, slog.New(slog.NewTextHandler(io.Discard, nil)), 5*time.Second, time.Minute).
		WithBaseURL(dead.URL)

	got, _ := reg.Locate(context.Background(), "5.161.158.200")
	if got.Ownership != OwnedUnknown {
		t.Fatalf("ownership = %s, want unknown — a failed read is not proof the address is someone else's",
			got.Ownership)
	}
}

func TestLimitReachedIsNotAnAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"resource_limit_exceeded","message":"server limit reached"}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, "tok")
	_, err := c.CreateFromSnapshot(context.Background(), CreateSpec{Name: "x", ServerType: "cpx31", Image: "1"})
	if !IsLimitReached(err) {
		t.Fatalf("a full project was not recognised: %v", err)
	}
	if IsAuthFailure(err) {
		t.Fatalf("a full project was read as a bad token: %v", err)
	}
}
