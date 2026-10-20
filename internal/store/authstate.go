package store

import (
	"encoding/base64"
	"fmt"
	"time"
)

// AuthState persists 2FA material (no plaintext secrets except pending brief window in memory).
type AuthState struct {
	TOTPBlob            string    `json:"totpBlob,omitempty"`       // base64(v1||nonce||ct)
	TOTPSalt            string    `json:"totpSalt,omitempty"`       // base64 16 bytes
	RecoveryHashes      []string  `json:"recoveryHashes,omitempty"` // sha256 hex
	PasswordFingerprint string    `json:"passwordFingerprint,omitempty"`
	Enrolled            bool      `json:"enrolled,omitempty"`
	LastTOTPStep        int64     `json:"lastTotpStep,omitempty"`
	BreakGlassDone      bool      `json:"breakGlassDone,omitempty"`   // marker: wipe already ran
	PendingSecretEnc    string    `json:"pendingSecretEnc,omitempty"` // encrypted pending enroll
	PendingExpires      time.Time `json:"pendingExpires,omitempty"`
}

func (s *Store) GetAuthState() AuthState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Auth
}

func (s *Store) SetAuthState(a AuthState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Auth = a
	return s.save()
}

func (s *Store) UpdateAuthState(fn func(*AuthState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(&s.st.Auth); err != nil {
		return err
	}
	return s.save()
}

func EncodeB64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
func DecodeB64(s string) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("empty")
	}
	return base64.StdEncoding.DecodeString(s)
}
