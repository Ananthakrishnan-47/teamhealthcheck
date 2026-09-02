package tokencrypto_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/agopalakrishnan/teams360/backend/pkg/tokencrypto"
)

// testKey is a deterministic 32-byte key. Test-only.
func testKey() string {
	return base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
}

func newCipher(t *testing.T) *tokencrypto.Cipher {
	t.Helper()
	c, err := tokencrypto.NewFromBase64(testKey())
	if err != nil {
		t.Fatalf("NewFromBase64() error = %v", err)
	}
	return c
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	c := newCipher(t)

	for _, plaintext := range []string{
		"podiq-api-token-value",
		"",
		strings.Repeat("x", 4096),
		"unicode ✓ token",
	} {
		encrypted, err := c.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("Encrypt(%q) error = %v", plaintext, err)
		}

		decrypted, err := c.Decrypt(encrypted)
		if err != nil {
			t.Fatalf("Decrypt() error = %v", err)
		}
		if decrypted != plaintext {
			t.Errorf("round trip = %q, want %q", decrypted, plaintext)
		}
	}
}

func TestEncryptDoesNotLeakPlaintext(t *testing.T) {
	c := newCipher(t)
	plaintext := "super-secret-podiq-token"

	encrypted, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	if strings.Contains(encrypted, plaintext) {
		t.Error("ciphertext contains the plaintext")
	}

	// Also check the decoded bytes, in case base64 obscured a substring match.
	raw, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		t.Fatalf("ciphertext is not base64: %v", err)
	}
	if strings.Contains(string(raw), plaintext) {
		t.Error("decoded ciphertext contains the plaintext")
	}
}

func TestEncryptUsesFreshNonce(t *testing.T) {
	c := newCipher(t)

	first, err := c.Encrypt("same-value")
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	second, err := c.Encrypt("same-value")
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	if first == second {
		t.Error("encrypting the same value twice produced identical ciphertext; nonce is not random")
	}
}

func TestDecryptWithWrongKeyFails(t *testing.T) {
	c := newCipher(t)

	encrypted, err := c.Encrypt("podiq-api-token-value")
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	other, err := tokencrypto.NewFromBase64(base64.StdEncoding.EncodeToString([]byte("ffffffffffffffffffffffffffffffff")))
	if err != nil {
		t.Fatalf("NewFromBase64() error = %v", err)
	}

	if _, err := other.Decrypt(encrypted); !errors.Is(err, tokencrypto.ErrInvalidCiphertext) {
		t.Errorf("Decrypt with wrong key error = %v, want ErrInvalidCiphertext", err)
	}
}

func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	c := newCipher(t)

	encrypted, err := c.Encrypt("podiq-api-token-value")
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	raw, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		t.Fatalf("ciphertext is not base64: %v", err)
	}
	raw[len(raw)-1] ^= 0xFF
	tampered := base64.StdEncoding.EncodeToString(raw)

	if _, err := c.Decrypt(tampered); !errors.Is(err, tokencrypto.ErrInvalidCiphertext) {
		t.Errorf("Decrypt of tampered ciphertext error = %v, want ErrInvalidCiphertext", err)
	}
}

func TestDecryptRejectsMalformedInput(t *testing.T) {
	c := newCipher(t)

	for name, input := range map[string]string{
		"not base64":         "!!!not-base64!!!",
		"empty":              "",
		"shorter than nonce": base64.StdEncoding.EncodeToString([]byte("short")),
	} {
		if _, err := c.Decrypt(input); !errors.Is(err, tokencrypto.ErrInvalidCiphertext) {
			t.Errorf("%s: Decrypt() error = %v, want ErrInvalidCiphertext", name, err)
		}
	}
}

func TestNilCipherIsNotConfigured(t *testing.T) {
	var c *tokencrypto.Cipher

	if _, err := c.Encrypt("value"); !errors.Is(err, tokencrypto.ErrNotConfigured) {
		t.Errorf("nil Encrypt() error = %v, want ErrNotConfigured", err)
	}
	if _, err := c.Decrypt("value"); !errors.Is(err, tokencrypto.ErrNotConfigured) {
		t.Errorf("nil Decrypt() error = %v, want ErrNotConfigured", err)
	}
}

func TestNewFromBase64RejectsWrongKeySize(t *testing.T) {
	if _, err := tokencrypto.NewFromBase64(base64.StdEncoding.EncodeToString([]byte("too-short"))); err == nil {
		t.Error("NewFromBase64() with a short key should fail")
	}
	if _, err := tokencrypto.NewFromBase64("not base64 at all %%%"); err == nil {
		t.Error("NewFromBase64() with invalid base64 should fail")
	}
}

func TestLoadReadsEnvironment(t *testing.T) {
	t.Setenv(tokencrypto.EnvKey, "")
	if tokencrypto.Load() != nil {
		t.Error("Load() with unset key should return nil (feature disabled)")
	}

	t.Setenv(tokencrypto.EnvKey, "this-is-not-a-valid-key")
	if tokencrypto.Load() != nil {
		t.Error("Load() with a malformed key should return nil rather than a broken cipher")
	}

	t.Setenv(tokencrypto.EnvKey, testKey())
	c := tokencrypto.Load()
	if c == nil {
		t.Fatal("Load() with a valid key returned nil")
	}
	if _, err := c.Encrypt("value"); err != nil {
		t.Errorf("cipher from Load() failed to encrypt: %v", err)
	}
}
