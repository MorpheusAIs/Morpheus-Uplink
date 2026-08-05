// Package keys implements API key generation and verification.
//
// Two kinds of keys exist:
//
//   - The master key is derived deterministically from API_KEY_SEED via
//     HMAC-SHA256. It is never stored: it can be re-derived after a full
//     storage wipe, which is the survival story for ephemeral VPS storage.
//   - Additional keys are random, stored only as SHA-256 hashes, and are
//     lost (revoked implicitly) if storage is wiped.
package keymaker

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"time"
)

const (
	// MasterKeyID identifies the derived full-access key in usage records.
	MasterKeyID = "master"
	// PromptKeyID identifies the derived inference-only key.
	PromptKeyID = "prompt"
)

func derive(seed, label, prefix string) string {
	mac := hmac.New(sha256.New, []byte(seed))
	mac.Write([]byte(label))
	return prefix + hex.EncodeToString(mac.Sum(nil))[:48]
}

// MasterKey derives the deterministic owner key from the seed. It is valid
// for both inference (/v1/*) and administration (/admin/*, /node/*).
func MasterKey(seed string) string {
	return derive(seed, "uplink-master-key-v1", "sk-uplink.")
}

// PromptKey derives the deterministic inference-only key from the seed.
// Safe default to paste into clients: it can prompt but cannot manage keys,
// sessions, or reach the node API.
func PromptKey(seed string) string {
	return derive(seed, "uplink-prompt-key-v1", "sk-prompt.")
}

// Record is the stored (non-secret) form of a generated API key.
type Record struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"` // first characters, for display
	Hash      string    `json:"hash"`   // SHA-256 hex of the full key
	CreatedAt time.Time `json:"createdAt"`
}

// NewKey generates a random API key. The full key is returned once and
// only its hash is retained in the Record.
func NewKey(name string) (full string, rec Record, err error) {
	idBytes := make([]byte, 4)
	secretBytes := make([]byte, 24)
	if _, err = rand.Read(idBytes); err != nil {
		return "", Record{}, fmt.Errorf("generate key id: %w", err)
	}
	if _, err = rand.Read(secretBytes); err != nil {
		return "", Record{}, fmt.Errorf("generate key secret: %w", err)
	}
	id := hex.EncodeToString(idBytes)
	full = "sk-" + id + "." + hex.EncodeToString(secretBytes)
	rec = Record{
		ID:        id,
		Name:      name,
		Prefix:    full[:12],
		Hash:      Hash(full),
		CreatedAt: time.Now().UTC(),
	}
	return full, rec, nil
}

// Hash returns the SHA-256 hex digest of a full key.
func Hash(full string) string {
	sum := sha256.Sum256([]byte(full))
	return hex.EncodeToString(sum[:])
}

// Equal compares a presented key against the master key in constant time.
func Equal(presented, master string) bool {
	return subtle.ConstantTimeCompare([]byte(presented), []byte(master)) == 1
}
