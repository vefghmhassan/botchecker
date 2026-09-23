package settings

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func newProvider(t *testing.T, defaults map[string]string) *Provider {
	t.Helper()
	dir := t.TempDir()

	db, err := sql.Open("sqlite", filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value BLOB,
		secret INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("schema: %v", err)
	}

	p, err := New(db, "", filepath.Join(dir, "secret.key"), defaults)
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	return p
}

func TestPrecedencePanelOverEnvOverDefault(t *testing.T) {
	p := newProvider(t, map[string]string{HetznerServerType: "cpx11"})

	if got := p.Get(HetznerServerType); got != "cpx11" {
		t.Fatalf("default = %q, want cpx11", got)
	}
	if got := p.Source(HetznerServerType); got != SourceDefault {
		t.Errorf("source = %s, want default", got)
	}

	t.Setenv("HETZNER_SERVER_TYPE", "cax21")
	if got := p.Get(HetznerServerType); got != "cax21" {
		t.Fatalf("env = %q, want cax21", got)
	}
	if got := p.Source(HetznerServerType); got != SourceEnv {
		t.Errorf("source = %s, want env", got)
	}

	// A value entered in the panel must win, otherwise it could never be
	// changed on a deployment that sets the variable.
	if err := p.Set(HetznerServerType, "ccx13"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := p.Get(HetznerServerType); got != "ccx13" {
		t.Fatalf("panel = %q, want ccx13", got)
	}
	if got := p.Source(HetznerServerType); got != SourcePanel {
		t.Errorf("source = %s, want panel", got)
	}
}

func TestClearFallsBackToEnv(t *testing.T) {
	p := newProvider(t, nil)
	t.Setenv("HETZNER_LOCATION", "nbg1")

	if err := p.Set(HetznerLocation, "ash"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := p.Clear(HetznerLocation); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := p.Get(HetznerLocation); got != "nbg1" {
		t.Fatalf("after clear = %q, want the env value back", got)
	}
}

func TestSecretsRoundTripEncrypted(t *testing.T) {
	p := newProvider(t, nil)
	const token = "hetzner-secret-value-1234"

	if err := p.Set(HetznerToken, token); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := p.Get(HetznerToken); got != token {
		t.Fatalf("Get = %q, want the stored token back", got)
	}

	// The plaintext must not be sitting in the table.
	var raw []byte
	if err := p.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, HetznerToken).Scan(&raw); err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatal("the token is stored in plaintext; a copy of the database would leak it")
	}
}

func TestViewsNeverExposeSecrets(t *testing.T) {
	p := newProvider(t, nil)
	const token = "bot-token-abcd1234"
	if err := p.Set(TelegramBotToken, token); err != nil {
		t.Fatalf("set: %v", err)
	}

	for _, v := range p.Views() {
		if v.Key != TelegramBotToken {
			continue
		}
		if v.Value != "" {
			t.Errorf("View.Value = %q, want empty for a secret", v.Value)
		}
		if !v.Configured {
			t.Error("View.Configured = false, want true")
		}
		if v.Hint != "…1234" {
			t.Errorf("View.Hint = %q, want the last four characters", v.Hint)
		}
		if strings.Contains(v.Hint, "bot-token") {
			t.Error("the hint leaks the token")
		}
		return
	}
	t.Fatal("the telegram token is missing from Views()")
}

func TestUndecryptableSecretIsTreatedAsUnset(t *testing.T) {
	p := newProvider(t, nil)
	if err := p.Set(HetznerToken, "value"); err != nil {
		t.Fatalf("set: %v", err)
	}

	// Simulate a lost key: overwrite the ciphertext with junk.
	if _, err := p.db.Exec(`UPDATE settings SET value = ? WHERE key = ?`,
		[]byte("not-a-valid-box"), HetznerToken); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if err := p.reload(); err != nil {
		t.Fatalf("reload must not fail on an undecryptable value: %v", err)
	}
	if p.Configured(HetznerToken) {
		t.Error("an undecryptable secret should read as unset so it can be entered again")
	}
}

func TestValidation(t *testing.T) {
	p := newProvider(t, nil)

	cases := []struct{ key, value string }{
		{OutageThreshold, "zero"},
		{OutageThreshold, "0"},
		{WatchInterval, "8 minutes"},
		{ProvisionEnabled, "yes please"},
		{XUIBaseURL, "panel.example.com"},
	}
	for _, tc := range cases {
		if err := p.Set(tc.key, tc.value); err == nil {
			t.Errorf("Set(%s, %q) was accepted, want a validation error", tc.key, tc.value)
		}
	}

	if err := p.Set(XUIBaseURL, "https://panel.example.com:2053"); err != nil {
		t.Errorf("a valid URL was rejected: %v", err)
	}
	if err := p.Set(ProvisionEnabled, "true"); err != nil {
		t.Errorf("a valid bool was rejected: %v", err)
	}
}

func TestUnknownKeyIsRejected(t *testing.T) {
	p := newProvider(t, nil)
	if err := p.Set("nope.not.a.key", "x"); err == nil {
		t.Fatal("Set accepted an unregistered key")
	}
}

func TestHint(t *testing.T) {
	if got := Hint(""); got != "" {
		t.Errorf("Hint(\"\") = %q, want empty", got)
	}
	if got := Hint("abcdefgh"); got != "…efgh" {
		t.Errorf("Hint = %q, want …efgh", got)
	}
	if got := Hint("ab"); strings.Contains(got, "ab") {
		t.Errorf("Hint(%q) = %q leaks a short secret", "ab", got)
	}
}
