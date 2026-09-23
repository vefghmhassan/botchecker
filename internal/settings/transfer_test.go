package settings

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func exportFile(t *testing.T, p *Provider, passphrase string) *Export {
	t.Helper()
	e, err := p.Export(passphrase, time.Now())
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	// Through JSON and back, the way the file actually travels.
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseExport(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return parsed
}

func mustSet(t *testing.T, p *Provider, key, value string) {
	t.Helper()
	if err := p.Set(key, value); err != nil {
		t.Fatalf("set %s: %v", key, err)
	}
}

// The whole point: one install's settings, tokens included, reproduced on
// another that has a different encryption key.
func TestSettingsAndTokensMoveWithAPassphrase(t *testing.T) {
	from := newProvider(t, map[string]string{HetznerServerType: "cpx11"})
	mustSet(t, from, TelegramChatID, "100000001")
	mustSet(t, from, TelegramBotToken, "123:secret-token")
	mustSet(t, from, ProvisionEnabled, "true")
	t.Setenv("WATCH_INTERVAL", "2m")

	e := exportFile(t, from, "correct horse")
	if strings.Contains(mustJSON(t, e), "secret-token") {
		t.Fatal("the token is readable in the file")
	}
	if _, ok := e.Values[HetznerServerType]; ok {
		t.Error("a built-in default was exported; it would pin the other install to it")
	}

	to := newProvider(t, nil)
	res, err := to.Import(e, "correct horse")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	for key, want := range map[string]string{
		TelegramChatID:   "100000001",
		TelegramBotToken: "123:secret-token",
		ProvisionEnabled: "true",
		WatchInterval:    "2m", // came from the environment, not the panel
	} {
		if got := to.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
		if to.Source(key) != SourcePanel {
			t.Errorf("%s was not stored", key)
		}
	}
	if len(res.Applied) != 4 {
		t.Errorf("applied %v, want 4 keys", res.Applied)
	}

	// Importing the same file again changes nothing.
	again, err := to.Import(e, "correct horse")
	if err != nil || len(again.Applied) != 0 || again.Unchanged != 4 {
		t.Errorf("second import: %+v, %v", again, err)
	}
}

func TestWithoutAPassphraseNoTokenLeavesAndTheFileSaysWhich(t *testing.T) {
	from := newProvider(t, nil)
	mustSet(t, from, TelegramChatID, "100000001")
	mustSet(t, from, TelegramBotToken, "123:secret-token")

	e := exportFile(t, from, "")
	if e.Secrets != nil || strings.Contains(mustJSON(t, e), "secret-token") {
		t.Fatal("a token was exported without a passphrase")
	}
	if len(e.LeftOut) != 1 || e.LeftOut[0] != TelegramBotToken {
		t.Errorf("left out = %v, want the bot token named", e.LeftOut)
	}

	to := newProvider(t, nil)
	res, err := to.Import(e, "")
	if err != nil {
		t.Fatal(err)
	}
	if to.Get(TelegramChatID) != "100000001" || to.Get(TelegramBotToken) != "" {
		t.Errorf("chat=%q token=%q", to.Get(TelegramChatID), to.Get(TelegramBotToken))
	}
	if len(res.LeftOut) != 1 {
		t.Errorf("the import did not report the missing token: %+v", res)
	}
}

func TestAShortPassphraseIsRefused(t *testing.T) {
	p := newProvider(t, nil)
	mustSet(t, p, TelegramBotToken, "123:secret-token")
	if _, err := p.Export("short", time.Now()); !errors.Is(err, ErrShortPassphrase) {
		t.Errorf("err = %v, want ErrShortPassphrase", err)
	}
}

// A wrong passphrase must not leave the install half imported: the plain
// values are just as much part of the file as the tokens.
func TestAWrongPassphraseChangesNothing(t *testing.T) {
	from := newProvider(t, nil)
	mustSet(t, from, TelegramChatID, "100000001")
	mustSet(t, from, TelegramBotToken, "123:secret-token")
	e := exportFile(t, from, "correct horse")

	to := newProvider(t, nil)
	for _, pass := range []string{"", "wrong horse"} {
		_, err := to.Import(e, pass)
		if err == nil {
			t.Fatalf("passphrase %q was accepted", pass)
		}
		if to.Get(TelegramChatID) != "" {
			t.Fatalf("passphrase %q: the chat id was imported anyway", pass)
		}
	}
}

func TestOneBadValueChangesNothing(t *testing.T) {
	e := &Export{Format: ExportFormat, Version: 1, Values: map[string]string{
		TelegramChatID:   "100000001",
		ProvisionEnabled: "sometimes",
	}}
	to := newProvider(t, nil)
	if _, err := to.Import(e, ""); err == nil {
		t.Fatal("an invalid boolean was accepted")
	}
	if to.Get(TelegramChatID) != "" {
		t.Error("the valid half of a bad file was applied")
	}
}

// A token pasted into the plain part by hand would be stored in the clear.
func TestASecretInThePlainPartIsRefused(t *testing.T) {
	e := &Export{Format: ExportFormat, Version: 1, Values: map[string]string{TelegramBotToken: "123:x"}}
	if _, err := newProvider(t, nil).Import(e, ""); err == nil {
		t.Fatal("a secret in the plain part was accepted")
	}
}

func TestUnknownKeysAreSkippedNotFatal(t *testing.T) {
	e := &Export{Format: ExportFormat, Version: 1, Values: map[string]string{
		TelegramChatID:  "100000001",
		"future.option": "on",
	}}
	to := newProvider(t, nil)
	res, err := to.Import(e, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Unknown) != 1 || to.Get(TelegramChatID) != "100000001" {
		t.Errorf("res = %+v", res)
	}
}

func TestSomethingElseIsNotAnExport(t *testing.T) {
	for _, data := range []string{`{}`, `{"format":"other"}`, `not json`} {
		if _, err := ParseExport([]byte(data)); !errors.Is(err, ErrNotAnExport) {
			t.Errorf("%s: err = %v", data, err)
		}
	}
	if _, err := ParseExport([]byte(`{"format":"botchecker-settings","version":99}`)); err == nil {
		t.Error("a file from a newer format was accepted")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
