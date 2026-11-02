package pool

import (
	"encoding/json"
	"math/big"
	"os"
	"testing"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// Spec §5 test 11, Go side: the open's check and the GUI marker
// (scripts/check-gui-js.sh reads the same vectors) agree on equal, +1 wei
// and off.
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
	// supply 1, budget 3600: StMorPerHourWei equals the price per second.
	in := StakeInputs{Supply: big.NewInt(1), Budget: big.NewInt(3600)}
	for _, v := range vs {
		w, _ := new(big.Int).SetString(v.W, 10)
		var limit *big.Int
		if v.Limit != nil {
			limit, _ = new(big.Int).SetString(*v.Limit, 10)
		}
		got, err := router.StMorPerHourWei(in.Supply, in.Budget, w)
		if err != nil || got.Cmp(w) != 0 {
			t.Fatalf("%s: setup %v %v", v.Case, got, err)
		}
		if over := !withinLimit(router.RatedBid{PricePerSecond: w}, in, limit); over != v.Over {
			t.Fatalf("%s: over=%v want %v", v.Case, over, v.Over)
		}
	}
}
