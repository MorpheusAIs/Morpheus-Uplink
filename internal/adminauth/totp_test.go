package adminauth

import (
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

func TestValidateTOTPRejectsSkewReplay(t *testing.T) {
	key, err := GenerateTOTP("Morpheus-Uplink", "admin")
	if err != nil {
		t.Fatal(err)
	}
	secret := key.Secret()
	now := time.Now().UTC()
	cur := now.Unix() / 30

	// Code for step cur-1 (still valid under ±1 skew at "now").
	past := time.Unix((cur-1)*30, 0).UTC()
	code, err := totp.GenerateCodeCustom(secret, past, totpOpts)
	if err != nil {
		t.Fatal(err)
	}

	ok, matched, err := ValidateTOTP(secret, code, 0)
	if err != nil || !ok {
		t.Fatalf("first accept: ok=%v err=%v", ok, err)
	}
	if matched != cur-1 {
		t.Fatalf("matched=%d want %d", matched, cur-1)
	}

	// Same code again at wall-clock still in skew of that step must fail.
	ok2, matched2, err2 := ValidateTOTP(secret, code, matched)
	if err2 == nil && ok2 {
		t.Fatalf("replay accepted: matched=%d", matched2)
	}
	if matched2 != cur-1 {
		t.Fatalf("replay matched step=%d want %d", matched2, cur-1)
	}
}

func TestValidateTOTPCurrentStep(t *testing.T) {
	key, err := GenerateTOTP("Morpheus-Uplink", "admin")
	if err != nil {
		t.Fatal(err)
	}
	secret := key.Secret()
	now := time.Now().UTC()
	code, err := totp.GenerateCodeCustom(secret, now, totpOpts)
	if err != nil {
		t.Fatal(err)
	}
	ok, matched, err := ValidateTOTP(secret, code, 0)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	cur := now.Unix() / 30
	if matched != cur {
		t.Fatalf("matched=%d want %d", matched, cur)
	}
	ok2, _, _ := ValidateTOTP(secret, code, matched)
	if ok2 {
		t.Fatal("same-step replay should fail")
	}
}
