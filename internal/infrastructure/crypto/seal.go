package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
)

type Sealer struct {
	key [32]byte
}

func NewSealer(secretMaterial []byte) (*Sealer, error) {
	if len(secretMaterial) == 0 {
		return nil, errors.New("crypto: secret material cannot be empty")
	}
	return &Sealer{key: sha256.Sum256(secretMaterial)}, nil
}

func (s *Sealer) Seal(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func (s *Sealer) Open(sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(sealed) < ns {
		return nil, errors.New("crypto: ciphertext too short")
	}
	return gcm.Open(nil, sealed[:ns], sealed[ns:], nil)
}
