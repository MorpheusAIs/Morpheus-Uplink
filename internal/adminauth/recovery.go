package adminauth

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// Recovery codes.
//
// Format: 12 symbols from the RFC 4648 base32 alphabet (a-z, 2-7), shown
// lowercase as xxxx-xxxx-xxxx. Each symbol is 5 bits from crypto/rand, so a
// code carries 60 bits. Input is normalized (case, spaces and dashes are
// ignored) before hashing.
//
// Storage: Uplink keeps no stored admin password hash (ADMIN_PASSWORD is
// compared from env), so there is no existing KDF to reuse. Each code is
// stored as HMAC-SHA256(pepper, salt || normalized code) with a random
// 16-byte per-code salt. The pepper is derived with HKDF from API_KEY_SEED,
// so the state file alone is not enough to test guesses offline. With 60
// bits of entropy per code a slow KDF would add little.
const (
	RecoveryCodeCount   = 10
	recoveryCodeSymbols = 12
	recoveryGroup       = 4
	recoveryAlphabet    = "abcdefghijklmnopqrstuvwxyz234567"
	recoveryInfo        = "uplink-recovery-v1"
	legacyCodeSymbols   = 16
)

// RecoveryHash is one stored code: hex salt and hex HMAC.
type RecoveryHash struct {
	Salt string `json:"salt"`
	Hash string `json:"hash"`
}

// RecoveryPepper derives the HMAC key for recovery codes from API_KEY_SEED.
func RecoveryPepper(apiKeySeed string) ([]byte, error) {
	return hkdf.Key(sha256.New, []byte(apiKeySeed), nil, recoveryInfo, 32)
}

// NewRecoveryCode returns one code in display form (xxxx-xxxx-xxxx).
func NewRecoveryCode() (string, error) {
	raw := make([]byte, recoveryCodeSymbols)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	var b strings.Builder
	for i, v := range raw {
		if i > 0 && i%recoveryGroup == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(recoveryAlphabet[v&31]) // 256 is a multiple of 32: uniform
	}
	return b.String(), nil
}

// NormalizeRecoveryCode lowercases and drops spaces and dashes.
func NormalizeRecoveryCode(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	return strings.NewReplacer("-", "", " ", "").Replace(code)
}

// LooksLikeRecoveryCode reports whether input has a recovery-code shape
// after normalization: 12 base32 symbols, or 16 for codes issued before
// salted storage.
func LooksLikeRecoveryCode(code string) bool {
	n := NormalizeRecoveryCode(code)
	if len(n) != recoveryCodeSymbols && len(n) != legacyCodeSymbols {
		return false
	}
	for i := 0; i < len(n); i++ {
		if !strings.ContainsRune(recoveryAlphabet, rune(n[i])) {
			return false
		}
	}
	return true
}

func recoveryMAC(pepper, salt []byte, normalized string) []byte {
	m := hmac.New(sha256.New, pepper)
	m.Write(salt)
	m.Write([]byte(normalized))
	return m.Sum(nil)
}

// HashRecoveryCode hashes one code with a fresh random salt.
func HashRecoveryCode(pepper []byte, code string) (RecoveryHash, error) {
	salt, err := RandomBytes(16)
	if err != nil {
		return RecoveryHash{}, err
	}
	sum := recoveryMAC(pepper, salt, NormalizeRecoveryCode(code))
	return RecoveryHash{Salt: hex.EncodeToString(salt), Hash: hex.EncodeToString(sum)}, nil
}

// GenerateRecoveryCodes returns n display codes and their stored hashes.
func GenerateRecoveryCodes(pepper []byte, n int) ([]string, []RecoveryHash, error) {
	plain := make([]string, 0, n)
	hashes := make([]RecoveryHash, 0, n)
	for i := 0; i < n; i++ {
		c, err := NewRecoveryCode()
		if err != nil {
			return nil, nil, err
		}
		h, err := HashRecoveryCode(pepper, c)
		if err != nil {
			return nil, nil, err
		}
		plain = append(plain, c)
		hashes = append(hashes, h)
	}
	return plain, hashes, nil
}

// MatchRecoveryCode returns the index of the stored hash that matches code,
// or -1. Every stored hash is checked with a constant-time compare.
func MatchRecoveryCode(pepper []byte, stored []RecoveryHash, code string) (int, error) {
	if len(pepper) == 0 {
		return -1, fmt.Errorf("empty pepper")
	}
	n := NormalizeRecoveryCode(code)
	match := -1
	for i, h := range stored {
		salt, err1 := hex.DecodeString(h.Salt)
		want, err2 := hex.DecodeString(h.Hash)
		if err1 != nil || err2 != nil || len(want) != sha256.Size {
			continue
		}
		got := recoveryMAC(pepper, salt, n)
		if subtle.ConstantTimeCompare(got, want) == 1 && match < 0 {
			match = i
		}
	}
	return match, nil
}

// MatchLegacyRecoveryCode checks codes issued before salted storage
// (unsalted SHA-256 hex of the uppercase code). Returns the index or -1.
func MatchLegacyRecoveryCode(stored []string, code string) int {
	c := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
	got := []byte(HashString(c))
	match := -1
	for i, h := range stored {
		if subtle.ConstantTimeCompare(got, []byte(h)) == 1 && match < 0 {
			match = i
		}
	}
	return match
}
