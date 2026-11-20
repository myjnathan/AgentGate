// Package secretbox encrypts secrets at rest (AES-256-GCM).
// Upstream API keys are never stored in plaintext and never returned
// by the admin API (the blueprint's "no plaintext secrets" baseline,
// fixing a known New API weakness).
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

type Box struct {
	gcm cipher.AEAD
}

// New derives a 32-byte key from the master secret.
func New(master string) (*Box, error) {
	if len(master) < 16 {
		return nil, errors.New("master secret must be at least 16 characters")
	}
	key := sha256.Sum256([]byte(master))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{gcm: gcm}, nil
}

func (b *Box) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, b.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := b.gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(out), nil
}

func (b *Box) Decrypt(enc string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("decode secret: %w", err)
	}
	ns := b.gcm.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	pt, err := b.gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt secret: %w", err)
	}
	return string(pt), nil
}

// Mask returns a display-safe mask like "sk-…abcd".
func Mask(key string) string {
	if len(key) <= 4 {
		return "****"
	}
	return "****" + key[len(key)-4:]
}
