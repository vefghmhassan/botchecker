// Package settings stores runtime configuration that can be changed from the
// dashboard, falling back to environment variables.
package settings

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrUndecryptable is returned when a stored secret cannot be opened with the
// current key — almost always because the key file was lost or replaced.
var ErrUndecryptable = errors.New("stored secret cannot be decrypted with the current key")

// sealer encrypts secret values at rest so that a copy of the database file,
// or a backup of it, does not hand over the Hetzner and Telegram tokens.
type sealer struct{ aead cipher.AEAD }

// newSealer loads the key from hexKey, or from keyPath, generating and storing
// a new one there (0600) on first run.
func newSealer(hexKey, keyPath string) (*sealer, error) {
	key, err := loadOrCreateKey(hexKey, keyPath)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sealer{aead: aead}, nil
}

func loadOrCreateKey(hexKey, keyPath string) ([]byte, error) {
	if hexKey = strings.TrimSpace(hexKey); hexKey != "" {
		key, err := hex.DecodeString(hexKey)
		if err != nil {
			return nil, fmt.Errorf("ENCRYPTION_KEY is not valid hex: %w", err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("ENCRYPTION_KEY must be 32 bytes (64 hex chars), got %d", len(key))
		}
		return key, nil
	}

	if raw, err := os.ReadFile(keyPath); err == nil {
		key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("key file %s is corrupt; delete it and re-enter the tokens", keyPath)
		}
		return key, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if dir := filepath.Dir(keyPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		return nil, fmt.Errorf("could not persist the generated key to %s: %w", keyPath, err)
	}
	return key, nil
}

func (s *sealer) seal(plaintext string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

func (s *sealer) open(box []byte) (string, error) {
	n := s.aead.NonceSize()
	if len(box) < n {
		return "", ErrUndecryptable
	}
	plain, err := s.aead.Open(nil, box[:n], box[n:], nil)
	if err != nil {
		return "", ErrUndecryptable
	}
	return string(plain), nil
}

// Hint renders the tail of a secret so the dashboard can show which value is
// configured without revealing it.
func Hint(secret string) string {
	if secret == "" {
		return ""
	}
	if len(secret) <= 4 {
		return "…" + strings.Repeat("•", len(secret))
	}
	return "…" + secret[len(secret)-4:]
}
