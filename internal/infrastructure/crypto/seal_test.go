package crypto

import (
	"bytes"
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
	ct2, err := s.Seal([]byte("super-secret-value"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ct1, ct2) {
		t.Fatal("identical plaintexts produced identical ciphertexts (nonce reuse)")
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
