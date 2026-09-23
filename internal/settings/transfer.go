package settings

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"
)

// Moving every setting from one install to another in a single file.
//
// The database cannot simply be copied across: its secrets are sealed with a
// key that lives beside it, and a copy without that key silently loses every
// token. So the export is its own format. Ordinary values are written as they
// are, and secrets only travel when the person exporting chooses a passphrase —
// then they are sealed with a key derived from it, so the file is useless to
// whoever finds it in a chat history or a downloads folder.

// ExportFormat names the file, so an unrelated JSON document is refused.
const ExportFormat = "botchecker-settings"

// exportVersion is bumped if the layout ever changes incompatibly.
const exportVersion = 1

// MinPassphrase is the shortest passphrase secrets may be sealed with.
const MinPassphrase = 8

var (
	ErrNotAnExport     = errors.New("this file is not a botchecker settings export")
	ErrWrongPassphrase = errors.New("the passphrase does not open the secrets in this file")
	ErrNeedPassphrase  = errors.New("this file carries encrypted tokens: enter the passphrase it was exported with")
	ErrShortPassphrase = fmt.Errorf("the passphrase must be at least %d characters", MinPassphrase)
)

// Export is the file.
type Export struct {
	Format     string            `json:"format"`
	Version    int               `json:"version"`
	ExportedAt time.Time         `json:"exported_at"`
	Values     map[string]string `json:"values"`
	Secrets    *SealedSecrets    `json:"secrets,omitempty"`
	// LeftOut names the secrets that were set but not exported, so the person
	// importing knows exactly what they still have to enter by hand.
	LeftOut []string `json:"secrets_left_out,omitempty"`
}

// SealedSecrets is the secret values as one AES-GCM box, keyed by scrypt.
type SealedSecrets struct {
	KDF   string `json:"kdf"`
	N     int    `json:"n"`
	R     int    `json:"r"`
	P     int    `json:"p"`
	Salt  string `json:"salt"`
	Nonce string `json:"nonce"`
	Box   string `json:"box"`
}

// scrypt cost: about a tenth of a second and 32 MB, once per export or import.
const scryptN, scryptR, scryptP = 1 << 15, 8, 1

// Export writes every setting that has a value other than the built-in
// default: the ones set in the panel and the ones coming from the
// environment. Both are included because the point is for the other install to
// behave the same, and it will not have this one's environment.
//
// With an empty passphrase no secret is written at all.
func (p *Provider) Export(passphrase string, now time.Time) (*Export, error) {
	e := &Export{Format: ExportFormat, Version: exportVersion, ExportedAt: now.UTC(), Values: map[string]string{}}
	secrets := map[string]string{}

	for _, def := range Defs {
		if p.Source(def.Key) == SourceDefault {
			continue
		}
		value := p.Get(def.Key)
		if value == "" {
			continue
		}
		if def.Secret() {
			secrets[def.Key] = value
			continue
		}
		e.Values[def.Key] = value
	}

	if len(secrets) == 0 {
		return e, nil
	}
	if passphrase == "" {
		for k := range secrets {
			e.LeftOut = append(e.LeftOut, k)
		}
		sort.Strings(e.LeftOut)
		return e, nil
	}
	if len(passphrase) < MinPassphrase {
		return nil, ErrShortPassphrase
	}
	sealed, err := sealSecrets(secrets, passphrase)
	if err != nil {
		return nil, err
	}
	e.Secrets = sealed
	return e, nil
}

// ImportResult says what an import changed.
type ImportResult struct {
	// Applied are the keys whose value changed.
	Applied []string
	// Unchanged counts the keys that already had the imported value.
	Unchanged int
	// Unknown are keys this version does not recognise — from a newer
	// install, most likely — and were skipped rather than refused.
	Unknown []string
	// LeftOut are secrets the exporting install had but did not include.
	LeftOut []string
}

// ParseExport reads a file and checks it is one of ours.
func ParseExport(data []byte) (*Export, error) {
	var e Export
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotAnExport, err)
	}
	if e.Format != ExportFormat {
		return nil, ErrNotAnExport
	}
	if e.Version > exportVersion {
		return nil, fmt.Errorf("this file was written by a newer botchecker (format %d); update this one first", e.Version)
	}
	return &e, nil
}

// Import applies a file. Everything is decrypted and validated before the
// first value is written, so a wrong passphrase or one bad value changes
// nothing rather than leaving the install half imported.
func (p *Provider) Import(e *Export, passphrase string) (ImportResult, error) {
	var res ImportResult
	incoming := map[string]string{}
	for k, v := range e.Values {
		incoming[k] = v
	}

	if e.Secrets != nil {
		if passphrase == "" {
			return res, ErrNeedPassphrase
		}
		secrets, err := openSecrets(e.Secrets, passphrase)
		if err != nil {
			return res, err
		}
		for k, v := range secrets {
			incoming[k] = v
		}
	}
	res.LeftOut = append(res.LeftOut, e.LeftOut...)

	keys := make([]string, 0, len(incoming))
	for k := range incoming {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	type change struct{ key, value string }
	var changes []change
	for _, k := range keys {
		def, ok := Lookup(k)
		if !ok {
			res.Unknown = append(res.Unknown, k)
			continue
		}
		value := strings.TrimSpace(incoming[k])
		if def.Secret() != isSecretIn(e, k) {
			// A key that moved between the plain and the sealed part would
			// mean the file was edited by hand; the safe reading is to refuse.
			return ImportResult{}, fmt.Errorf("%s is in the wrong part of the file", k)
		}
		if err := validate(def, value); err != nil {
			return ImportResult{}, err
		}
		if p.Get(k) == value && p.Source(k) == SourcePanel {
			res.Unchanged++
			continue
		}
		changes = append(changes, change{k, value})
	}

	for _, c := range changes {
		if err := p.Set(c.key, c.value); err != nil {
			return res, fmt.Errorf("stopped at %s after applying %d: %w", c.key, len(res.Applied), err)
		}
		res.Applied = append(res.Applied, c.key)
	}
	return res, nil
}

func isSecretIn(e *Export, key string) bool {
	_, plain := e.Values[key]
	return !plain
}

func sealSecrets(secrets map[string]string, passphrase string) (*SealedSecrets, error) {
	plain, err := json.Marshal(secrets)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	aead, err := passphraseAEAD(passphrase, salt, scryptN, scryptR, scryptP)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	b64 := base64.StdEncoding.EncodeToString
	return &SealedSecrets{
		KDF: "scrypt", N: scryptN, R: scryptR, P: scryptP,
		Salt: b64(salt), Nonce: b64(nonce),
		Box: b64(aead.Seal(nil, nonce, plain, []byte(ExportFormat))),
	}, nil
}

func openSecrets(s *SealedSecrets, passphrase string) (map[string]string, error) {
	if s.KDF != "scrypt" {
		return nil, fmt.Errorf("unsupported key derivation %q", s.KDF)
	}
	// Bounds, so a crafted file cannot make the import allocate gigabytes.
	if s.N < 1<<10 || s.N > 1<<20 || s.R < 1 || s.R > 32 || s.P < 1 || s.P > 16 {
		return nil, errors.New("the file's key-derivation parameters are out of range")
	}
	dec := base64.StdEncoding.DecodeString
	salt, err1 := dec(s.Salt)
	nonce, err2 := dec(s.Nonce)
	box, err3 := dec(s.Box)
	if err := errors.Join(err1, err2, err3); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotAnExport, err)
	}
	aead, err := passphraseAEAD(passphrase, salt, s.N, s.R, s.P)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, ErrNotAnExport
	}
	plain, err := aead.Open(nil, nonce, box, []byte(ExportFormat))
	if err != nil {
		return nil, ErrWrongPassphrase
	}
	var out map[string]string
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotAnExport, err)
	}
	return out, nil
}

func passphraseAEAD(passphrase string, salt []byte, n, r, p int) (cipher.AEAD, error) {
	key, err := scrypt.Key([]byte(passphrase), salt, n, r, p, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
