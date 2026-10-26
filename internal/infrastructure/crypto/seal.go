package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/hkdf"
)

const (
	DefaultKeyID    = "default"
	envelopeVersion = "v1:"
	tokenHashPrefix = "sha256:"
	dekSize         = 32
)

// Sealer provides authenticated AES-256-GCM envelope encryption (KEK/DEK) with
// Associated Data (AAD) and multi-key rotation support.
//
// Envelope ciphertexts are framed as:
//
//	v1:<key-id>:<hex-wrapped-dek>:<nonce+ciphertext>
//
// Legacy unprefixed AES-256-GCM ciphertexts (`nonce || ciphertext || tag`) remain
// readable via Open / OpenWithAAD for seamless backwards compatibility.
type Sealer struct {
	activeKeyID string
	keks        map[string]cipher.AEAD
	kekOrder    []string
}

// NewSealer derives a single-key Sealer (with key ID "default") from secret.
func NewSealer(secret []byte) (*Sealer, error) {
	if len(secret) == 0 {
		return nil, errors.New("encryption secret must not be empty")
	}
	return NewKeyring(DefaultKeyID, map[string][]byte{
		DefaultKeyID: secret,
	})
}

// NewKeyring constructs a multi-key Sealer where activeKeyID is used to seal
// new ciphertexts and all keys in secrets can open historical ciphertexts.
func NewKeyring(activeKeyID string, secrets map[string][]byte) (*Sealer, error) {
	activeKeyID = strings.TrimSpace(activeKeyID)
	if activeKeyID == "" {
		return nil, errors.New("active key ID must not be empty")
	}
	if strings.Contains(activeKeyID, ":") {
		return nil, errors.New("key ID must not contain ':'")
	}
	if len(secrets) == 0 {
		return nil, errors.New("keyring must contain at least one key")
	}
	if len(secrets[activeKeyID]) == 0 {
		return nil, fmt.Errorf("active key %q not found or empty in keyring", activeKeyID)
	}

	keks := make(map[string]cipher.AEAD, len(secrets))
	order := make([]string, 0, len(secrets))
	order = append(order, activeKeyID)

	for id, raw := range secrets {
		id = strings.TrimSpace(id)
		if id == "" || strings.Contains(id, ":") {
			return nil, fmt.Errorf("invalid key ID %q", id)
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("secret for key %q must not be empty", id)
		}
		aead, err := deriveKEK(raw)
		if err != nil {
			return nil, err
		}
		keks[id] = aead
		if id != activeKeyID {
			order = append(order, id)
		}
	}

	return &Sealer{
		activeKeyID: activeKeyID,
		keks:        keks,
		kekOrder:    order,
	}, nil
}

// ParseKeyring builds a Sealer from an optional ENCRYPTION_KEYRING environment
// string ("k2:secret2,k1:secret1"), an optional ENCRYPTION_ACTIVE_KEY_ID, and
// a fallback secret (e.g., CONTEXT_ENCRYPTION_KEY).
func ParseKeyring(keyringEnv, activeKeyID string, fallbackSecret []byte) (*Sealer, error) {
	keyringEnv = strings.TrimSpace(keyringEnv)
	activeKeyID = strings.TrimSpace(activeKeyID)
	if keyringEnv == "" {
		return NewSealer(fallbackSecret)
	}

	secrets := make(map[string][]byte)
	var firstID string
	for _, entry := range strings.Split(keyringEnv, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, rawSecret, ok := strings.Cut(entry, ":")
		id = strings.TrimSpace(id)
		if !ok || id == "" || rawSecret == "" {
			return nil, fmt.Errorf("invalid ENCRYPTION_KEYRING entry (expected <key-id>:<secret>)")
		}
		if firstID == "" {
			firstID = id
		}
		secrets[id] = []byte(rawSecret)
	}
	if len(secrets) == 0 {
		return nil, errors.New("ENCRYPTION_KEYRING did not contain any valid keys")
	}
	if activeKeyID == "" {
		activeKeyID = firstID
	}
	if len(fallbackSecret) > 0 {
		if _, exists := secrets[DefaultKeyID]; !exists {
			secrets[DefaultKeyID] = fallbackSecret
		}
	}
	return NewKeyring(activeKeyID, secrets)
}

func deriveKEK(secret []byte) (cipher.AEAD, error) {
	key := make([]byte, 32)
	r := hkdf.New(sha256.New, secret, nil, []byte("infra-guy/context-sealer/v1"))
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, err
	}
	return newGCM(key)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// ActiveKeyID returns the ID of the KEK currently used to seal new ciphertexts.
func (s *Sealer) ActiveKeyID() string {
	if s == nil {
		return ""
	}
	return s.activeKeyID
}

// Seal encrypts plaintext using envelope encryption without additional AAD.
func (s *Sealer) Seal(plaintext []byte) ([]byte, error) {
	return s.SealWithAAD(plaintext, nil)
}

// SealWithAAD encrypts plaintext with a randomly generated 32-byte DEK using
// AES-256-GCM bound to aad, wraps the DEK with the active KEK (also bound to aad),
// and returns a versioned v1 envelope.
func (s *Sealer) SealWithAAD(plaintext, aad []byte) ([]byte, error) {
	if s == nil {
		return nil, errors.New("sealer is nil")
	}
	kek, ok := s.keks[s.activeKeyID]
	if !ok {
		return nil, fmt.Errorf("active key %q not loaded", s.activeKeyID)
	}

	dek := make([]byte, dekSize)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, err
	}
	dekAEAD, err := newGCM(dek)
	if err != nil {
		return nil, err
	}

	dekNonce := make([]byte, kek.NonceSize())
	if _, err := io.ReadFull(rand.Reader, dekNonce); err != nil {
		return nil, err
	}
	wrappedDEK := kek.Seal(dekNonce, dekNonce, dek, aad)
	wrappedDEKHex := hex.EncodeToString(wrappedDEK)

	payloadNonce := make([]byte, dekAEAD.NonceSize())
	if _, err := io.ReadFull(rand.Reader, payloadNonce); err != nil {
		return nil, err
	}
	sealedPayload := dekAEAD.Seal(payloadNonce, payloadNonce, plaintext, aad)

	header := envelopeVersion + s.activeKeyID + ":" + wrappedDEKHex + ":"
	out := make([]byte, len(header)+len(sealedPayload))
	copy(out, header)
	copy(out[len(header):], sealedPayload)
	return out, nil
}

// Open decrypts ciphertext without AAD.
func (s *Sealer) Open(ciphertext []byte) ([]byte, error) {
	return s.OpenWithAAD(ciphertext, nil)
}

// OpenWithAAD decrypts either a v1 envelope ciphertext or a legacy raw AES-256-GCM
// ciphertext. For v1 envelopes, aad must match the AAD used during SealWithAAD.
func (s *Sealer) OpenWithAAD(ciphertext, aad []byte) ([]byte, error) {
	if s == nil {
		return nil, errors.New("sealer is nil")
	}
	if keyID, wrappedDEK, payload, ok := parseEnvelope(ciphertext); ok {
		kek, exists := s.keks[keyID]
		if !exists {
			return nil, fmt.Errorf("unknown encryption key ID %q", keyID)
		}
		ns := kek.NonceSize()
		if len(wrappedDEK) < ns+kek.Overhead() {
			return nil, errors.New("wrapped DEK too short")
		}
		dek, err := kek.Open(nil, wrappedDEK[:ns], wrappedDEK[ns:], aad)
		if err != nil {
			return nil, err
		}
		dekAEAD, err := newGCM(dek)
		if err != nil {
			return nil, err
		}
		pns := dekAEAD.NonceSize()
		if len(payload) < pns+dekAEAD.Overhead() {
			return nil, errors.New("ciphertext payload too short")
		}
		return dekAEAD.Open(nil, payload[:pns], payload[pns:], aad)
	}

	// Legacy fallback: raw AES-256-GCM (`nonce || ciphertext || tag`).
	var lastErr error
	for _, id := range s.kekOrder {
		kek := s.keks[id]
		ns := kek.NonceSize()
		if len(ciphertext) < ns+kek.Overhead() {
			lastErr = errors.New("ciphertext too short")
			continue
		}
		pt, err := kek.Open(nil, ciphertext[:ns], ciphertext[ns:], aad)
		if err == nil {
			return pt, nil
		}
		if len(aad) > 0 {
			// Legacy rows created before AAD binding was introduced used nil AAD.
			if ptLegacy, errLegacy := kek.Open(nil, ciphertext[:ns], ciphertext[ns:], nil); errLegacy == nil {
				return ptLegacy, nil
			}
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("unable to decrypt ciphertext")
}

// IsEnvelopeCiphertext reports whether b has a valid v1 envelope header.
func IsEnvelopeCiphertext(b []byte) bool {
	_, _, _, ok := parseEnvelope(b)
	return ok
}

// EnvelopeKeyID extracts the KEK ID from a v1 envelope ciphertext.
func EnvelopeKeyID(b []byte) (string, bool) {
	keyID, _, _, ok := parseEnvelope(b)
	return keyID, ok
}

// NeedsRotation reports whether ciphertext is either in the legacy unprefixed
// format or sealed with a non-active KEK ID.
func (s *Sealer) NeedsRotation(ciphertext []byte) bool {
	if s == nil || len(ciphertext) == 0 {
		return false
	}
	keyID, ok := EnvelopeKeyID(ciphertext)
	if !ok {
		return true
	}
	return keyID != s.activeKeyID
}

func parseEnvelope(b []byte) (keyID string, wrappedDEK []byte, payload []byte, ok bool) {
	if !bytes.HasPrefix(b, []byte(envelopeVersion)) {
		return "", nil, nil, false
	}
	rest := b[len(envelopeVersion):]
	idx1 := bytes.IndexByte(rest, ':')
	if idx1 <= 0 || idx1 > 64 {
		return "", nil, nil, false
	}
	keyID = string(rest[:idx1])
	rest = rest[idx1+1:]
	idx2 := bytes.IndexByte(rest, ':')
	// Wrapped DEK is 12-byte nonce + 32-byte DEK + 16-byte tag = 60 bytes = 120 hex chars.
	if idx2 != 120 {
		return "", nil, nil, false
	}
	wrappedHex := rest[:idx2]
	wrappedDEK = make([]byte, 60)
	if _, err := hex.Decode(wrappedDEK, wrappedHex); err != nil {
		return "", nil, nil, false
	}
	payload = rest[idx2+1:]
	if len(payload) < 28 {
		return "", nil, nil, false
	}
	return keyID, wrappedDEK, payload, true
}

// HashToken returns a deterministic SHA-256 hex digest prefixed with "sha256:".
// If token is already a "sha256:" digest or empty, it is returned unchanged.
func HashToken(token string) string {
	if token == "" {
		return ""
	}
	if strings.HasPrefix(token, tokenHashPrefix) && len(token) == len(tokenHashPrefix)+64 {
		return token
	}
	sum := sha256.Sum256([]byte(token))
	return tokenHashPrefix + hex.EncodeToString(sum[:])
}

// VerifyTokenHash compares a bearer token against an expected "sha256:<hex>"
// digest in constant time.
func VerifyTokenHash(token, expectedHash string) bool {
	if token == "" || expectedHash == "" {
		return false
	}
	computed := HashToken(token)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(expectedHash)) == 1
}
