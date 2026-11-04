package pool

import (
	"encoding/json"
	"math/big"
	"os"
	"testing"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// Spec §5 test 11: the open's stake check is strict > against the cap:
// equal passes, +1 wei is over, and off never is.
func TestStakeLimitSpec11MarkerParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/stake_marker_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []struct {
		Case  string  `json:"case"`
		Limit *string `json:"limit"`
		W     string  `json:"w"`
		Over  bool    `json:"over"`
	}
	if err := json.Unmarshal(raw, &vs); err != nil || len(vs) == 0 {
		t.Fatalf("vectors: %v", err)
	}
	// supply 1, budget 601: a 600 s stake equals the price per second.
	in := StakeInputs{Supply: big.NewInt(1), Budget: big.NewInt(601)}
	for _, v := range vs {
		w, _ := new(big.Int).SetString(v.W, 10)
		var limit *big.Int
		if v.Limit != nil {
			limit, _ = new(big.Int).SetString(*v.Limit, 10)
		}
		got := bidStake(router.RatedBid{PricePerSecond: w}, in, 600)
		if got == nil || got.Cmp(w) != 0 {
			t.Fatalf("%s: setup %v", v.Case, got)
		}
		if over := limit != nil && got.Cmp(limit) > 0; over != v.Over {
			t.Fatalf("%s: over=%v want %v", v.Case, over, v.Over)
		}
	}
}
