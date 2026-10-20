package adminauth

import (
	"regexp"
	"strings"
	"testing"
)

func TestRecoveryCodeFormatAndEntropy(t *testing.T) {
	re := regexp.MustCompile(`^[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4}$`)
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		c, err := NewRecoveryCode()
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(c) {
			t.Fatalf("bad format %q", c)
		}
		if seen[c] {
			t.Fatalf("duplicate code %q", c)
		}
		seen[c] = true
	}
	// 12 symbols x 5 bits.
	if recoveryCodeSymbols*5 < 50 {
		t.Fatal("recovery codes must carry at least 50 bits")
	}
}

func TestRecoveryCodeHashAndMatch(t *testing.T) {
	pepper, err := RecoveryPepper("seed")
	if err != nil {
		t.Fatal(err)
	}
	plain, hashes, err := GenerateRecoveryCodes(pepper, 10)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range plain {
		if strings.Contains(hashes[i].Hash, strings.ReplaceAll(c, "-", "")) {
			t.Fatal("hash contains the code")
		}
		variants := []string{c, strings.ToUpper(c), strings.ReplaceAll(c, "-", ""), " " + strings.ReplaceAll(c, "-", " ") + " "}
		for _, v := range variants {
			if got, _ := MatchRecoveryCode(pepper, hashes, v); got != i {
				t.Fatalf("match(%q)=%d want %d", v, got, i)
			}
		}
	}
	if got, _ := MatchRecoveryCode(pepper, hashes, "aaaa-aaaa-aaaa"); got != -1 {
		t.Fatalf("unknown code matched %d", got)
	}
	other, _ := RecoveryPepper("other-seed")
	if got, _ := MatchRecoveryCode(other, hashes, plain[0]); got != -1 {
		t.Fatal("code matched under a different pepper")
	}
	// Same code, two hashes: salts differ.
	h1, _ := HashRecoveryCode(pepper, plain[0])
	h2, _ := HashRecoveryCode(pepper, plain[0])
	if h1.Salt == h2.Salt || h1.Hash == h2.Hash {
		t.Fatal("per-code salt not applied")
	}
}

func TestLooksLikeRecoveryCode(t *testing.T) {
	for in, want := range map[string]bool{
		"abcd-efgh-2345": true, "ABCDEFGH2345": true, "123456": false,
		"abcd-efgh-234": false, "ABCDEFGHIJKLMNOP": false, "abcd-efgh-2341": false, "": false,
	} {
		if got := LooksLikeRecoveryCode(in); got != want {
			t.Fatalf("LooksLikeRecoveryCode(%q)=%v want %v", in, got, want)
		}
	}
}
