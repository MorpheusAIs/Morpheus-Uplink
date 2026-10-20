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
	// RecoveryCodes are salted HMACs of the current recovery codes
	// (see adminauth/recovery.go). Single use: a used entry is removed.
	RecoveryCodes       []adminauth.RecoveryHash `json:"recoveryCodes,omitempty"`
	PasswordFingerprint string                   `json:"passwordFingerprint,omitempty"`
	Enrolled            bool                     `json:"enrolled,omitempty"`
	LastTOTPStep        int64                    `json:"lastTotpStep,omitempty"`
	BreakGlassDone      bool                     `json:"breakGlassDone,omitempty"`   // marker: wipe already ran
	PendingSecretEnc    string                   `json:"pendingSecretEnc,omitempty"` // encrypted pending enroll
	PendingExpires      time.Time                `json:"pendingExpires,omitempty"`

	// TwoFactorOn is the 2FA switch (Security tab), default off. Set in the
	// same write that stores a confirmed secret (enroll confirm) and cleared
	// in the same write that wipes it (/admin/2fa/remove). Reset wipes the
	// secret but keeps it set, so admin routes allow only re-enrollment.
	TwoFactorOn bool `json:"twoFactorOn"`
}

// On reports whether 2FA is on. A stored secret counts as on even if the
// flag is missing or false (fail closed for a hand-edited or partial file).
func (a AuthState) On() bool {
	return a.TwoFactorOn || a.Enrolled
}

// RecoveryRemaining is the number of unused recovery codes.
func (a AuthState) RecoveryRemaining() int {
	return len(a.RecoveryCodes)
}

// GetAuthState returns a copy; slices are cloned so callers cannot mutate
// the stored state without UpdateAuthState.
func (s *Store) GetAuthState() AuthState {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.st.Auth
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
