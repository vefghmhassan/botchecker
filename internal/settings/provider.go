package settings

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Source says where a value came from, so the settings page can show it.
type Source string

const (
	SourcePanel   Source = "panel"
	SourceEnv     Source = "env"
	SourceDefault Source = "default"
)

// Provider resolves settings with panel values taking precedence over the
// environment. Clients read through it on every call rather than capturing
// credentials at construction, so a token changed in the dashboard takes
// effect immediately without a restart.
type Provider struct {
	db       *sql.DB
	seal     *sealer
	defaults map[string]string

	mu     sync.RWMutex
	stored map[string]string // resolved plaintext, panel-provided only
	loaded bool
}

// New opens the settings store. keyPath is where the generated encryption key
// is kept when ENCRYPTION_KEY is not set.
func New(db *sql.DB, hexKey, keyPath string, defaults map[string]string) (*Provider, error) {
	s, err := newSealer(hexKey, keyPath)
	if err != nil {
		return nil, err
	}
	if defaults == nil {
		defaults = map[string]string{}
	}
	p := &Provider{db: db, seal: s, defaults: defaults}
	return p, p.reload()
}

func (p *Provider) reload() error {
	rows, err := p.db.Query(`SELECT key, value, secret FROM settings`)
	if err != nil {
		return err
	}
	defer rows.Close()

	stored := map[string]string{}
	for rows.Next() {
		var key string
		var raw []byte
		var secret int
		if err := rows.Scan(&key, &raw, &secret); err != nil {
			return err
		}
		if secret == 1 {
			plain, err := p.seal.open(raw)
			if err != nil {
				// A value we cannot decrypt is treated as unset rather than
				// failing startup: the operator can simply enter it again.
				continue
			}
			stored[key] = plain
			continue
		}
		stored[key] = string(raw)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	p.mu.Lock()
	p.stored = stored
	p.loaded = true
	p.mu.Unlock()
	return nil
}

// Get resolves a key: panel value, then environment, then default.
func (p *Provider) Get(key string) string {
	p.mu.RLock()
	v, ok := p.stored[key]
	p.mu.RUnlock()
	if ok && v != "" {
		return v
	}

	if def, found := Lookup(key); found && def.Env != "" {
		if env := strings.TrimSpace(os.Getenv(def.Env)); env != "" {
			return env
		}
	}
	return p.defaults[key]
}

// Source reports where Get would take the value from.
func (p *Provider) Source(key string) Source {
	p.mu.RLock()
	v, ok := p.stored[key]
	p.mu.RUnlock()
	if ok && v != "" {
		return SourcePanel
	}
	if def, found := Lookup(key); found && def.Env != "" {
		if strings.TrimSpace(os.Getenv(def.Env)) != "" {
			return SourceEnv
		}
	}
	return SourceDefault
}

func (p *Provider) Bool(key string) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(p.Get(key)))
	return err == nil && b
}

func (p *Provider) Int(key string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(p.Get(key)))
	if err != nil {
		return fallback
	}
	return n
}

func (p *Provider) Duration(key string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(p.Get(key)))
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// Configured reports whether a key resolves to anything at all.
func (p *Provider) Configured(key string) bool { return p.Get(key) != "" }

// Set stores a panel value, replacing whatever was there. Secrets are
// encrypted before they touch the database.
func (p *Provider) Set(key, value string) error {
	def, ok := Lookup(key)
	if !ok {
		return fmt.Errorf("unknown setting %q", key)
	}
	if err := validate(def, value); err != nil {
		return err
	}

	var stored []byte
	if def.Secret() {
		box, err := p.seal.seal(value)
		if err != nil {
			return err
		}
		stored = box
	} else {
		stored = []byte(value)
	}

	if _, err := p.db.Exec(`
		INSERT INTO settings (key, value, secret, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, secret = excluded.secret, updated_at = excluded.updated_at`,
		key, stored, boolToInt(def.Secret()), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return p.reload()
}

// Clear removes the panel value, letting the environment or default take over
// again. It is deliberately separate from Set so that submitting an empty form
// field cannot wipe a stored token by accident.
func (p *Provider) Clear(key string) error {
	if _, ok := Lookup(key); !ok {
		return fmt.Errorf("unknown setting %q", key)
	}
	if _, err := p.db.Exec(`DELETE FROM settings WHERE key = ?`, key); err != nil {
		return err
	}
	return p.reload()
}

func validate(def Def, value string) error {
	value = strings.TrimSpace(value)
	switch def.Kind {
	case KindBool:
		if _, err := strconv.ParseBool(value); err != nil {
			return fmt.Errorf("%s must be true or false", def.Key)
		}
	case KindInt:
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("%s must be a whole number", def.Key)
		}
		if n < 1 {
			return fmt.Errorf("%s must be at least 1", def.Key)
		}
	case KindDuration:
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("%s must be a duration such as 30m or 8m", def.Key)
		}
		if d <= 0 {
			return fmt.Errorf("%s must be positive", def.Key)
		}
	case KindChoice:
		allowed, ok := Choices[def.Key]
		if !ok {
			return nil
		}
		for _, a := range allowed {
			if value == a {
				return nil
			}
		}
		return fmt.Errorf("%s must be one of %s", def.Key, strings.Join(allowed, ", "))

	case KindString:
		if def.Key == XUIBaseURL && value != "" &&
			!strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
			return fmt.Errorf("%s must start with http:// or https://", def.Key)
		}
	}
	return nil
}

// View is the read contract for one setting. The value of a secret is never
// included — only whether it is set and a short hint, the same approach the
// 3x-ui panel takes with its own tokens.
type View struct {
	Key        string `json:"key"`
	Label      string `json:"label"`
	Help       string `json:"help"`
	Group      string `json:"group"`
	Kind       Kind   `json:"kind"`
	Secret     bool   `json:"secret"`
	Configured bool   `json:"configured"`
	Source     Source `json:"source"`
	Env        string `json:"env"`
	Value      string `json:"value,omitempty"` // empty for secrets
	Hint       string `json:"hint,omitempty"`  // secrets only
}

// Views returns every setting for display, grouped in registry order.
func (p *Provider) Views() []View {
	out := make([]View, 0, len(Defs))
	for _, def := range Defs {
		value := p.Get(def.Key)
		v := View{
			Key: def.Key, Label: def.Label, Help: def.Help, Group: def.Group,
			Kind: def.Kind, Secret: def.Secret(), Env: def.Env,
			Configured: value != "", Source: p.Source(def.Key),
		}
		if def.Secret() {
			v.Hint = Hint(value)
		} else {
			v.Value = value
		}
		out = append(out, v)
	}
	return out
}

// ViewsByGroup returns the settings of one group, for a single form.
func (p *Provider) ViewsByGroup(group string) []View {
	var out []View
	for _, v := range p.Views() {
		if v.Group == group {
			out = append(out, v)
		}
	}
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
