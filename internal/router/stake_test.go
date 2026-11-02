package router

import (
	"context"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
)

func bi(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("bad int " + s)
	}
	return n
}

// Spec §5 test 1: StakeForDurationWei is the router's formula,
// ceil(supply × pps × (D+1) ÷ budget), in integer math.
func TestStakeForDurationWeiFormula(t *testing.T) {
	cases := []struct {
		supply, budget, pps string
		d                   int64
		want                string
	}{
		// exact: 1e6 × 2 × 601 / 1e3 = 1_202_000
		{"1000000", "1000", "2", 600, "1202000"},
		// rounds up: 10 × 1 × 601 / 3 = 2003.33… → 2004
		{"10", "3", "1", 600, "2004"},
		// 1800 s at a large supply (no float rounding; checked independently)
		{"10000000000000000000000000", "3000000000000000000000", "123456789", 1800, "741152256630000"},
	}
	for _, c := range cases {
		got, err := StakeForDurationWei(bi(c.supply), bi(c.budget), bi(c.pps), c.d)
		if err != nil {
			t.Fatal(err)
		}
		if got.String() != c.want {
			t.Fatalf("StakeForDurationWei(%s,%s,%s,%d)=%s want %s", c.supply, c.budget, c.pps, c.d, got, c.want)
		}
	}
}

func TestStMorPerHourWei(t *testing.T) {
	// 1e6 × 2 × 3600 / 1e3 = 7_200_000 (exact)
	got, err := StMorPerHourWei(bi("1000000"), bi("1000"), bi("2"))
	if err != nil || got.String() != "7200000" {
		t.Fatalf("got %v %v", got, err)
	}
	// 10 × 1 × 3600 / 7 = 5142.86 → 5143
	got, err = StMorPerHourWei(bi("10"), bi("7"), bi("1"))
	if err != nil || got.String() != "5143" {
		t.Fatalf("ceil: got %v %v", got, err)
	}
}

// Each helper errors unless supply, budget and pps are all > 0.
func TestStakeHelpersRefuseNonPositive(t *testing.T) {
	one := big.NewInt(1)
	for _, bad := range []*big.Int{nil, big.NewInt(0), big.NewInt(-1)} {
		for i := 0; i < 3; i++ {
			in := []*big.Int{one, one, one}
			in[i] = bad
			if _, err := StMorPerHourWei(in[0], in[1], in[2]); err == nil {
				t.Fatalf("StMorPerHourWei accepted input %d = %v", i, bad)
			}
			if _, err := StakeForDurationWei(in[0], in[1], in[2], 600); err == nil {
				t.Fatalf("StakeForDurationWei accepted input %d = %v", i, bad)
			}
		}
	}
	if _, err := StakeForDurationWei(one, one, one, 0); err == nil {
		t.Fatal("zero duration accepted")
	}
}

// The rated-bids decode keeps each bid's PricePerSecond (string or number).
func TestRatedBidsKeepPricePerSecond(t *testing.T) {
	const bid1 = "0x1111111111111111111111111111111111111111111111111111111111111111"
	const bid2 = "0x2222222222222222222222222222222222222222222222222222222222222222"
	const bid3 = "0x3333333333333333333333333333333333333333333333333333333333333333"
	const prov = "0x4444444444444444444444444444444444444444"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"bids":[
{"Bid":{"Id":"` + bid1 + `","Provider":"` + prov + `","PricePerSecond":"20555555555555556"},"Score":3},
{"Bid":{"Id":"` + bid2 + `","Provider":"` + prov + `","PricePerSecond":1000},"Score":2},
{"Bid":{"Id":"` + bid3 + `","Provider":"` + prov + `"},"Score":1}]}`))
	}))
	defer ts.Close()
	rows, err := New(ts.URL, "u", "p").RatedBidsCtx(context.Background(), "0xm")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].PricePerSecond.String() != "20555555555555556" || rows[1].PricePerSecond.String() != "1000" || rows[2].PricePerSecond != nil {
		t.Fatalf("rows %+v", rows)
	}
}
