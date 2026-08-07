package router

import (
	"math/big"
	"testing"
)

func TestSessionStakeWei(t *testing.T) {
	supply := big.NewInt(1_000_000)
	budget := big.NewInt(1_000)
	pps := big.NewInt(2)
	// stake = supply * pps * duration / budget = 1e6 * 2 * 600 / 1e3 = 1_200_000
	got, err := SessionStakeWei(supply, budget, pps, 600)
	if err != nil {
		t.Fatal(err)
	}
	want := big.NewInt(1_200_000)
	if got.Cmp(want) != 0 {
		t.Fatalf("stake=%s want %s", got, want)
	}
}

func TestSessionStakeWeiZeroBudget(t *testing.T) {
	_, err := SessionStakeWei(big.NewInt(1), big.NewInt(0), big.NewInt(1), 600)
	if err == nil {
		t.Fatal("expected error for zero budget")
	}
}
