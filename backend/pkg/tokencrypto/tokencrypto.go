// Package tokencrypto provides symmetric, reversible protection for secrets that
// THC must be able to read back and present to a third party — currently the
// external organization-provider API token.
//
// This is deliberately distinct from password hashing. User passwords use bcrypt
// (one-way, verified by comparison and never recovered); a provider API token has
// to leave the database as plaintext to be sent upstream, so it needs encryption
// rather than hashing.
package tokencrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
)

// EnvKey names the environment variable holding the base64-encoded 32-byte key.
const EnvKey = "TOKEN_ENCRYPTION_KEY"

// KeySize is the required key length in bytes (AES-256).
const KeySize = 32

var (
	// ErrNotConfigured is returned when encryption is attempted without a key.
	ErrNotConfigured = errors.New("token encryption is not configured")
	// ErrInvalidCiphertext is returned when stored data cannot be decrypted.
	// It deliberately carries no detail about the failure to avoid leaking
	// information about the key or the plaintext.
	ErrInvalidCiphertext = errors.New("stored token could not be decrypted")
)

// Cipher encrypts and decrypts short secrets using AES-256-GCM.
// A nil *Cipher means encryption is disabled; callers should treat that as
// "provider integration not configured" rather than as a fatal error.
type Cipher struct {
	aead cipher.AEAD
}

// Load reads TOKEN_ENCRYPTION_KEY and returns a ready Cipher.
//
// It returns nil when the variable is unset, mirroring the nil-means-disabled
// convention used by email.LoadConfig and email.LoadSESConfig. It also returns
// nil when the key is present but unusable, so a malformed key disables the
// feature instead of panicking at startup; callers that need to tell the two
// apart should use NewFromBase64.
func Load() *Cipher {
	raw := os.Getenv(EnvKey)
	if raw == "" {
		return nil
	}

	c, err := NewFromBase64(raw)
	if err != nil {
		return nil
	}
	return c
}

// NewFromBase64 builds a Cipher from a base64-encoded 32-byte key.
// Standard and URL-safe base64 are both accepted, with or without padding.
func NewFromBase64(encoded string) (*Cipher, error) {
	key, err := decodeKey(encoded)
	if err != nil {
		return nil, err
	}

	if len(key) != KeySize {
		return nil, fmt.Errorf("%s must decode to %d bytes, got %d", EnvKey, KeySize, len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	return &Cipher{aead: aead}, nil
}

// decodeKey tries the base64 alphabets a deployment is likely to produce.
func decodeKey(encoded string) ([]byte, error) {
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}

	for _, enc := range encodings {
		if key, err := enc.DecodeString(encoded); err == nil {
			return key, nil
		}
	}

	return nil, fmt.Errorf("%s is not valid base64", EnvKey)
}

// Encrypt returns base64(nonce || ciphertext) for the given plaintext.
// A fresh random nonce is used every call, so encrypting the same value twice
// yields different output.
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	if c == nil {
		return "", ErrNotConfigured
	}

	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}

	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt.
//
// Every failure mode returns ErrInvalidCiphertext unchanged: distinguishing
// "bad base64" from "wrong key" from "tampered ciphertext" would hand an
// attacker an oracle, and the caller can do nothing different in any case.
func (c *Cipher) Decrypt(encoded string) (string, error) {
	if c == nil {
		return "", ErrNotConfigured
	}

	sealed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", ErrInvalidCiphertext
	}

	nonceSize := c.aead.NonceSize()
	if len(sealed) < nonceSize {
		return "", ErrInvalidCiphertext
	}

	nonce, ciphertext := sealed[:nonceSize], sealed[nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", ErrInvalidCiphertext
	}

	return string(plaintext), nil
}
