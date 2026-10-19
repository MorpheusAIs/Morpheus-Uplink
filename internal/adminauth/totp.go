package adminauth

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image/png"
	"time"

	"github.com/boombuler/barcode"
	"github.com/boombuler/barcode/qr"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// GenerateTOTP creates a new TOTP key for issuer/account.
func GenerateTOTP(issuer, account string) (*otp.Key, error) {
	return totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: account,
		Period:      30,
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1,
	})
}

// ValidateTOTP checks code with ±1 step. usedStep prevents step reuse (unix/period).
func ValidateTOTP(secret, code string, usedStep int64) (ok bool, step int64, err error) {
	step = time.Now().UTC().Unix() / 30
	if usedStep != 0 && step == usedStep {
		return false, step, fmt.Errorf("step reuse")
	}
	ok, err = totp.ValidateCustom(code, secret, time.Now().UTC(), totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		return false, step, err
	}
	return ok, step, nil
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
