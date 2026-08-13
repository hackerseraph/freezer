package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"golang.org/x/crypto/argon2"
)

// encryptionMagic is the 4-byte header identifying an encrypted Freezer file.
var encryptionMagic = []byte("FRZR")

// activeEncryptionKey holds the in-memory derived key for the current session.
// It is NEVER written to disk. Nil means encryption is locked / passphrase not entered.
var activeEncryptionKey []byte

// deriveKey runs Argon2id on the passphrase and salt to produce a 32-byte AES key.
func deriveKey(passphrase, saltB64 string) ([]byte, error) {
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return nil, fmt.Errorf("invalid encryption salt: %w", err)
	}
	key := argon2.IDKey([]byte(passphrase), salt, 3, 64*1024, 4, 32)
	return key, nil
}

// newEncryptionSalt generates a fresh random 32-byte salt and returns it as base64.
func newEncryptionSalt() (string, error) {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(salt), nil
}

// encryptStream reads plaintext from r, encrypts with AES-256-GCM, and returns the ciphertext.
// Format: [4-byte magic][1-byte version][12-byte nonce][ciphertext+16-byte tag]
func encryptStream(r io.Reader, key []byte) ([]byte, error) {
	plaintext, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)
	var buf bytes.Buffer
	buf.Write(encryptionMagic)
	buf.WriteByte(0x01)
	buf.Write(nonce)
	buf.Write(ciphertext)
	return buf.Bytes(), nil
}

// decryptBytes decrypts an AES-256-GCM encrypted blob produced by encryptStream.
func decryptBytes(data []byte, key []byte) ([]byte, error) {
	if len(data) < 4+1+12+16 {
		return nil, fmt.Errorf("encrypted file too short")
	}
	if !bytes.Equal(data[:4], encryptionMagic) {
		return nil, fmt.Errorf("not a Freezer encrypted file")
	}
	nonce := data[5:17]
	ciphertext := data[17:]

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
