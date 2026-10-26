package crypto

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"
)

func TestSealerRoundTripAndTamper(t *testing.T) {
	if _, err := NewSealer(nil); err == nil {
		t.Fatal("expected error for empty secret key material")
	}
	s, err := NewSealer([]byte("test-key-material"))
	if err != nil {
		t.Fatal(err)
	}
	ct1, err := s.Seal([]byte("super-secret-value"))
	if err != nil {
		t.Fatal(err)
	}
	if !IsEnvelopeCiphertext(ct1) {
		t.Fatalf("expected v1 envelope ciphertext, got %q", string(ct1[:16]))
	}
	if keyID, ok := EnvelopeKeyID(ct1); !ok || keyID != DefaultKeyID {
		t.Fatalf("EnvelopeKeyID = (%q, %v), want (%q, true)", keyID, ok, DefaultKeyID)
	}
	ct2, err := s.Seal([]byte("super-secret-value"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ct1, ct2) {
		t.Fatal("identical plaintexts produced identical ciphertexts (nonce/DEK reuse)")
	}
	pt, err := s.Open(ct1)
	if err != nil {
		t.Fatal(err)
	}
	if string(pt) != "super-secret-value" {
		t.Fatalf("opened %q, want super-secret-value", string(pt))
	}
	ct1[len(ct1)-1] ^= 0xff
	if _, err := s.Open(ct1); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}

func TestSealerAADBinding(t *testing.T) {
	s, err := NewSealer([]byte("test-aad-secret"))
	if err != nil {
		t.Fatal(err)
	}
	aad := []byte("context_entry:ctx-1:secret_env:AWS_SECRET_ACCESS_KEY")
	ct, err := s.SealWithAAD([]byte("top-secret"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ct); err == nil {
		t.Fatal("expected Open without AAD to fail for AAD-bound envelope")
	}
	if _, err := s.OpenWithAAD(ct, []byte("context_entry:ctx-2:secret_env:AWS_SECRET_ACCESS_KEY")); err == nil {
		t.Fatal("expected OpenWithAAD with wrong AAD to fail")
	}
	pt, err := s.OpenWithAAD(ct, aad)
	if err != nil {
		t.Fatalf("OpenWithAAD with matching AAD failed: %v", err)
	}
	if string(pt) != "top-secret" {
		t.Fatalf("opened %q, want top-secret", string(pt))
	}
}

func TestKeyringRotationAndLegacyBackwardsCompatibility(t *testing.T) {
	oldSecret := []byte("legacy-secret-v1")
	sOld, err := NewKeyring("k1", map[string][]byte{"k1": oldSecret})
	if err != nil {
		t.Fatal(err)
	}
	aad := []byte("project:proj-1:state")
	ctOld, err := sOld.SealWithAAD([]byte(`{"version":4}`), aad)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a legacy pre-envelope raw AES-256-GCM ciphertext (SHA-256 derived key).
	legacyAEAD, err := deriveLegacySHA256KEK(oldSecret)
	if err != nil {
		t.Fatal(err)
	}
	legacyNonce := make([]byte, legacyAEAD.NonceSize())
	if _, err := io.ReadFull(rand.Reader, legacyNonce); err != nil {
		t.Fatal(err)
	}
	ctLegacy := legacyAEAD.Seal(legacyNonce, legacyNonce, []byte("legacy-secret"), nil)
	if IsEnvelopeCiphertext(ctLegacy) {
		t.Fatal("legacy raw GCM ciphertext should not be detected as v1 envelope")
	}

	// Rotate to k2 while keeping k1 in the keyring.
	sRotated, err := ParseKeyring("k2:new-secret-v2,k1:legacy-secret-v1", "k2", oldSecret)
	if err != nil {
		t.Fatal(err)
	}
	if sRotated.ActiveKeyID() != "k2" {
		t.Fatalf("ActiveKeyID = %q, want k2", sRotated.ActiveKeyID())
	}
	if !sRotated.NeedsRotation(ctOld) {
		t.Fatal("expected k1 ciphertext to need rotation under active key k2")
	}
	if !sRotated.NeedsRotation(ctLegacy) {
		t.Fatal("expected legacy ciphertext to need rotation")
	}

	// Both k1 envelope and legacy raw GCM ciphertexts open cleanly with sRotated.
	ptOld, err := sRotated.OpenWithAAD(ctOld, aad)
	if err != nil {
		t.Fatalf("failed to open k1 ciphertext after rotation: %v", err)
	}
	if string(ptOld) != `{"version":4}` {
		t.Fatalf("ptOld = %q", string(ptOld))
	}
	ptLegacy, err := sRotated.OpenWithAAD(ctLegacy, aad)
	if err != nil {
		t.Fatalf("failed to open pre-AAD legacy ciphertext: %v", err)
	}
	if string(ptLegacy) != "legacy-secret" {
		t.Fatalf("ptLegacy = %q", string(ptLegacy))
	}

	// Re-seal with active key k2.
	ctNew, err := sRotated.SealWithAAD(ptOld, aad)
	if err != nil {
		t.Fatal(err)
	}
	if sRotated.NeedsRotation(ctNew) {
		t.Fatal("newly sealed k2 ciphertext should not need rotation")
	}
}

func TestHashTokenAndVerify(t *testing.T) {
	raw := "st-1234567890abcdef"
	h := HashToken(raw)
	if h == raw || len(h) != len("sha256:")+64 {
		t.Fatalf("unexpected hash %q", h)
	}
	if HashToken(h) != h {
		t.Fatal("HashToken should be idempotent on existing sha256: digests")
	}
	if !VerifyTokenHash(raw, h) {
		t.Fatal("VerifyTokenHash returned false for matching token")
	}
	if VerifyTokenHash("wrong-token", h) {
		t.Fatal("VerifyTokenHash returned true for wrong token")
	}
}
