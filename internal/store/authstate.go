package store

import (
	"encoding/base64"
	"fmt"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/adminauth"
)

// AuthState persists 2FA material (no plaintext secrets except pending brief window in memory).
type AuthState struct {
	TOTPBlob string `json:"totpBlob,omitempty"` // base64(v1||nonce||ct)
	TOTPSalt string `json:"totpSalt,omitempty"` // base64 16 bytes
	// RecoveryHashes are codes issued before salted storage (unsalted
	// sha256 hex); still accepted once each, cleared on regenerate/reset.
	RecoveryHashes []string `json:"recoveryHashes,omitempty"`
	// RecoveryCodes are salted HMACs of the current recovery codes
	// (see adminauth/recovery.go). Single use: a used entry is removed.
	RecoveryCodes       []adminauth.RecoveryHash `json:"recoveryCodes,omitempty"`
	PasswordFingerprint string                   `json:"passwordFingerprint,omitempty"`
	Enrolled            bool                     `json:"enrolled,omitempty"`
	LastTOTPStep        int64                    `json:"lastTotpStep,omitempty"`
	BreakGlassDone      bool                     `json:"breakGlassDone,omitempty"`   // marker: wipe already ran
	PendingSecretEnc    string                   `json:"pendingSecretEnc,omitempty"` // encrypted pending enroll
	PendingExpires      time.Time                `json:"pendingExpires,omitempty"`
}

// RecoveryRemaining is the number of unused recovery codes.
func (a AuthState) RecoveryRemaining() int {
	return len(a.RecoveryCodes) + len(a.RecoveryHashes)
}

// GetAuthState returns a copy; slices are cloned so callers cannot mutate
// the stored state without UpdateAuthState.
func (s *Store) GetAuthState() AuthState {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.st.Auth
	a.RecoveryHashes = append([]string(nil), a.RecoveryHashes...)
	a.RecoveryCodes = append([]adminauth.RecoveryHash(nil), a.RecoveryCodes...)
	return a
}

func (s *Store) SetAuthState(a AuthState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Auth = a
	return s.save()
}

// UpdateAuthState runs fn under the store lock and saves only if fn returns
// nil, so read-check-write sequences (consume a recovery code, advance the
// TOTP step) are atomic. On error the in-memory state is left unchanged.
func (s *Store) UpdateAuthState(fn func(*AuthState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.st.Auth
	a.RecoveryHashes = append([]string(nil), a.RecoveryHashes...)
	a.RecoveryCodes = append([]adminauth.RecoveryHash(nil), a.RecoveryCodes...)
	if err := fn(&a); err != nil {
		return err
	}
	prev := s.st.Auth
	s.st.Auth = a
	if err := s.save(); err != nil {
		s.st.Auth = prev
		return err
	}
	return nil
}

func EncodeB64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
func DecodeB64(s string) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("empty")
	}
	return base64.StdEncoding.DecodeString(s)
}
