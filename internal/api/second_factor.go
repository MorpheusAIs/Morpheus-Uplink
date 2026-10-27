package api

import (
	"errors"
	"strconv"
	"strings"

	"github.com/absgrafx/morpheus-uplink/internal/adminauth"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

var (
	errNotEnrolled  = errors.New("not enrolled")
	errSecondFactor = errors.New("second factor rejected")
)

type factorResult struct {
	ok        bool
	method    string // "totp" | "recovery"
	reason    string // failure reason for audit (never the code)
	remaining int    // unused recovery codes after this call
}

// consumeSecondFactor checks code against the TOTP secret or one unused
// recovery code and commits the result in one store update: a TOTP success
// advances LastTOTPStep (a step can never be used twice), a recovery
// success removes that code. Concurrent requests with the same code cannot
// both succeed. then, if non-nil, runs in the same update after a success
// (used by reset to wipe 2FA in the same write).
//
// A 6-digit input is treated as TOTP, a 12-symbol base32 input as a
// recovery code; anything else fails as bad_totp.
func (s *Server) consumeSecondFactor(code string, then func(*store.AuthState)) factorResult {
	return s.consumeFactor(code, true, then)
}

// consumeTOTP is consumeSecondFactor that accepts a TOTP code only. A
// recovery code is refused before the store update, so it is not consumed.
func (s *Server) consumeTOTP(code string, then func(*store.AuthState)) factorResult {
	return s.consumeFactor(code, false, then)
}

func (s *Server) consumeFactor(code string, allowRecovery bool, then func(*store.AuthState)) factorResult {
	var res factorResult
	code = strings.TrimSpace(code)
	if !allowRecovery && adminauth.LooksLikeRecoveryCode(code) {
		res.reason = "recovery_code_refused"
		return res
	}
	err := s.store.UpdateAuthState(func(a *store.AuthState) error {
		if !a.Enrolled {
			res.reason = "not_enrolled"
			return errNotEnrolled
		}
		if adminauth.LooksLikeRecoveryCode(code) {
			if !allowRecovery { // unreachable: refused above, kept as a guard
				res.reason = "recovery_code_refused"
				return errSecondFactor
			}
			pepper, err := adminauth.RecoveryPepper(s.cfg.APIKeySeed)
			if err != nil {
				res.reason = "recovery_error"
				return errSecondFactor
			}
			if i, _ := adminauth.MatchRecoveryCode(pepper, a.RecoveryCodes, code); i >= 0 {
				a.RecoveryCodes = append(a.RecoveryCodes[:i], a.RecoveryCodes[i+1:]...)
			} else {
				res.reason = "bad_recovery_code"
				return errSecondFactor
			}
			res.method = "recovery"
		} else {
			secret, err := s.decryptTOTPSecret(a.TOTPBlob, a.TOTPSalt)
			if err != nil {
				res.reason = "totp_decrypt"
				return errSecondFactor
			}
			ok, step, err := adminauth.ValidateTOTP(secret, code, a.LastTOTPStep)
			if err != nil || !ok {
				res.reason = "bad_totp"
				if err != nil && err.Error() == "step reuse" {
					res.reason = "totp_reuse"
				}
				return errSecondFactor
			}
			a.LastTOTPStep = step
			res.method = "totp"
		}
		if then != nil {
			then(a)
		}
		res.remaining = a.RecoveryRemaining()
		return nil
	})
	if err != nil {
		res.ok = false
		if res.reason == "" {
			res.reason = "save_failed"
		}
		return res
	}
	res.ok = true
	return res
}

// auditRecoveryUse records a consumed recovery code (count only, never the code).
func (s *Server) auditRecoveryUse(ip string, r factorResult) {
	if r.ok && r.method == "recovery" {
		s.auditEvent("recovery_code_used", ip, "ok", "remaining="+strconv.Itoa(r.remaining))
	}
}

// issueRecoveryCodes returns fresh display codes and their stored hashes.
func (s *Server) issueRecoveryCodes() ([]string, []adminauth.RecoveryHash, error) {
	pepper, err := adminauth.RecoveryPepper(s.cfg.APIKeySeed)
	if err != nil {
		return nil, nil, err
	}
	return adminauth.GenerateRecoveryCodes(pepper, adminauth.RecoveryCodeCount)
}
