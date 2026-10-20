package adminauth

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"image/png"
	"time"

	"github.com/boombuler/barcode"
	"github.com/boombuler/barcode/qr"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

var totpOpts = totp.ValidateOpts{
	Period:    30,
	Skew:      1,
	Digits:    otp.DigitsSix,
	Algorithm: otp.AlgorithmSHA1,
}

// GenerateTOTP creates a new TOTP key for issuer/account.
func GenerateTOTP(issuer, account string) (*otp.Key, error) {
	return totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: account,
		Period:      totpOpts.Period,
		Digits:      totpOpts.Digits,
		Algorithm:   totpOpts.Algorithm,
	})
}

// ValidateTOTP checks code with ±1 step skew. It returns the step that
// actually matched and requires matched > usedStep (no step reuse across
// the skew window). usedStep 0 means no prior successful step.
func ValidateTOTP(secret, code string, usedStep int64) (ok bool, matchedStep int64, err error) {
	now := time.Now().UTC()
	cur := now.Unix() / int64(totpOpts.Period)
	code = normalizeCode(code)
	if code == "" {
		return false, cur, fmt.Errorf("empty code")
	}
	// Try current, then past, then future so the matched step is the one
	// GenerateCodeCustom produced for that counter.
	candidates := []int64{cur, cur - 1, cur + 1}
	for _, step := range candidates {
		if step <= 0 {
			continue
		}
		t := time.Unix(step*int64(totpOpts.Period), 0).UTC()
		want, genErr := totp.GenerateCodeCustom(secret, t, totpOpts)
		if genErr != nil {
			return false, cur, genErr
		}
		if subtle.ConstantTimeCompare([]byte(code), []byte(want)) != 1 {
			continue
		}
		if usedStep != 0 && step <= usedStep {
			return false, step, fmt.Errorf("step reuse")
		}
		return true, step, nil
	}
	return false, cur, nil
}

func normalizeCode(code string) string {
	out := make([]byte, 0, len(code))
	for i := 0; i < len(code); i++ {
		c := code[i]
		if c >= '0' && c <= '9' {
			out = append(out, c)
		}
	}
	return string(out)
}

// QRPNGBase64 returns a data-URL-ready base64 PNG of the otpauth URL, or empty on failure.
func QRPNGBase64(otpauthURL string) string {
	code, err := qr.Encode(otpauthURL, qr.M, qr.Auto)
	if err != nil {
		return ""
	}
	code, err = barcode.Scale(code, 200, 200)
	if err != nil {
		return ""
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, code); err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}
