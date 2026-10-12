// Package credentials provides transparent at-rest encryption for host
// credentials (SSH passwords / private keys) stored in SQLite.
//
// The encryption key is derived from the platform JWT secret (same source as
// api/middleware): PROMLIFECYCLE_JWT_SECRET env, PROMLIFECYCLE_JWT_SECRET_FILE,
// or the default /opt/promlifecycle-server/jwt_secret. Losing/rotating that
// secret makes stored credentials undecryptable - keep the file backed up.
package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	encPrefix = "enc:v1:"
)

var (
	once sync.Once
	aead cipher.AEAD
)

// LoadJWTSecret returns the platform JWT signing secret, persisting a random
// one to disk on first run so tokens and encrypted credentials survive
// restarts. Returns ("", false) only when nothing can be persisted.
func LoadJWTSecret() (string, bool) {
	if s := os.Getenv("PROMLIFECYCLE_JWT_SECRET"); s != "" {
		return s, true
	}

	readOrCreate := func(path string) (string, bool) {
		if data, err := os.ReadFile(path); err == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				return s, true
			}
		}
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", false
		}
		s := hex.EncodeToString(b)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err == nil {
			if err := os.WriteFile(path, []byte(s), 0600); err == nil {
				return s, true
			}
		}
		return s, false
	}

	if p := os.Getenv("PROMLIFECYCLE_JWT_SECRET_FILE"); p != "" {
		if s, ok := readOrCreate(p); ok {
			return s, true
		}
	}
	const defaultPath = "/opt/promlifecycle-server/jwt_secret"
	return readOrCreate(defaultPath)
}

func initCipher() {
	secret, persistent := LoadJWTSecret()
	secret, err := resolveCipherSecret(secret, persistent, rand.Reader)
	if err != nil {
		panic("credentials: secure key generation: " + err.Error())
	}
	if !persistent {
		log.Printf("[Credentials] WARNING: no persistent JWT secret - encrypted credentials will be lost on restart")
	}
	sum := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		panic("credentials: aes: " + err.Error())
	}
	aead, err = cipher.NewGCM(block)
	if err != nil {
		panic("credentials: gcm: " + err.Error())
	}
}

func resolveCipherSecret(secret string, persistent bool, random io.Reader) (string, error) {
	if secret != "" {
		return secret, nil
	}
	if persistent {
		return "", errors.New("persistent JWT secret is empty")
	}
	randomSecret := make([]byte, 32)
	if _, err := io.ReadFull(random, randomSecret); err != nil {
		return "", err
	}
	return hex.EncodeToString(randomSecret), nil
}

// Encrypt encrypts plaintext for at-rest storage. Empty input is returned
// unchanged (no credential). Values already carrying the encryption prefix
// are returned as-is (idempotent for GORM BeforeSave re-runs).
func Encrypt(plain string) (string, error) {
	if plain == "" || strings.HasPrefix(plain, encPrefix) {
		return plain, nil
	}
	once.Do(initCipher)
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("credentials: nonce: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, []byte(plain), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. Legacy plaintext (no prefix) is returned as-is so
// existing databases keep working until the next write re-encrypts them.
func Decrypt(stored string) (string, error) {
	if stored == "" || !strings.HasPrefix(stored, encPrefix) {
		return stored, nil
	}
	once.Do(initCipher)
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encPrefix))
	if err != nil {
		return "", fmt.Errorf("credentials: decode: %w", err)
	}
	ns := aead.NonceSize()
	if len(raw) < ns {
		return "", fmt.Errorf("credentials: ciphertext too short")
	}
	open, err := aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("credentials: decrypt failed (JWT secret changed since the host was saved?): %w", err)
	}
	return string(open), nil
}
