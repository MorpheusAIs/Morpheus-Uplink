package chain

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestEncodeStakesOnHold(t *testing.T) {
	data, err := encodeStakesOnHold("0xC599884ACB421BD548D6fAe444CD075DFB012FBF", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(data, "0x967885df") {
		t.Fatalf("selector: %s", data[:10])
	}
	// address left-padded to 32 bytes + uint8 1
	wantSuffix := "000000000000000000000000c599884acb421bd548d6fae444cd075dfb012fbf" +
		"0000000000000000000000000000000000000000000000000000000000000001"
	if !strings.HasSuffix(strings.ToLower(data), wantSuffix) {
		t.Fatalf("encoding mismatch:\n%s\n…%s", data, wantSuffix)
	}
}

func TestNextUTCMidnight(t *testing.T) {
	now := time.Date(2026, 8, 6, 15, 30, 0, 0, time.UTC)
	next := NextUTCMidnight(now)
	if next.Year() != 2026 || next.Month() != 8 || next.Day() != 7 || next.Hour() != 0 {
		t.Fatalf("got %v", next)
	}
}

func TestEncodeWithdrawUserStakes(t *testing.T) {
	data, err := EncodeWithdrawUserStakes("0xC599884ACB421BD548D6fAe444CD075DFB012FBF", 255)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(data[:4]) != "a98a7c6b" {
		t.Fatalf("selector %x", data[:4])
	}
	if data[len(data)-1] != 255 {
		t.Fatalf("iterations byte %d", data[len(data)-1])
	}
}
