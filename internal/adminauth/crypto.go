package adminauth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"io"
)

const totpInfo = "uplink-totp-v1"

// DeriveTOTPKey returns a 32-byte AES key from API_KEY_SEED + install salt.
func DeriveTOTPKey(apiKeySeed string, salt []byte) ([]byte, error) {
	if len(salt) != 16 {
		return nil, fmt.Errorf("totp salt must be 16 bytes")
	}
	return hkdf.Key(sha256.New, []byte(apiKeySeed), salt, totpInfo, 32)
}

// EncryptSecret AES-GCM encrypts plaintext; output is v1||nonce(12)||ciphertext+tag.
func EncryptSecret(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
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
	ct := gcm.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 1+len(nonce)+len(ct))
	out[0] = 1
	copy(out[1:], nonce)
	copy(out[1+len(nonce):], ct)
	return out, nil
}

// DecryptSecret decrypts a v1||nonce||ct blob.
func DecryptSecret(key, blob []byte) ([]byte, error) {
	if len(blob) < 1+12+16 || blob[0] != 1 {
		return nil, fmt.Errorf("invalid totp blob")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	nonce := blob[1 : 1+nonceSize]
	ct := blob[1+nonceSize:]
	return gcm.Open(nil, nonce, ct, nil)
}

func HashToken(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func RandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return nil, err
	}
	return b, nil
}

func RandomBase32(nBytes int) (string, error) {
	b, err := RandomBytes(nBytes)
	if err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}

// PasswordFingerprint is HMAC-SHA256(seed, password) for change detection.
func PasswordFingerprint(apiKeySeed, password string) string {
	mac := hmac.New(sha256.New, []byte(apiKeySeed))
	_, _ = mac.Write([]byte(password))
	return hex.EncodeToString(mac.Sum(nil))
}
