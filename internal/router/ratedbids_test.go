package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The router's rated-bids rows carry the bid id in Bid.Id; the outer ID is
// left unset and comes out as the zero hash. A by-bid open must use Bid.Id,
// and rows with no usable id or a zero provider are skipped.
func TestRatedBidsUsesInnerBidID(t *testing.T) {
	const zero = "0x0000000000000000000000000000000000000000000000000000000000000000"
	const bid1 = "0x5d68945b58ee56a95521ff0e8ff38189d0bad9d9198c6d5daa5bea8c0a1f4f5f"
	const bid2 = "0x1111111111111111111111111111111111111111111111111111111111111111"
	var opened string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /blockchain/models/{id}/bids/rated", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"bids":[
			{"ID":"` + zero + `","Bid":{"Id":"` + bid1 + `","Provider":"0x559B16C3D329F058B5413B0009465E64DF66A16B"},"Score":2},
			{"ID":"` + zero + `","Bid":{"Id":"` + zero + `","Provider":"0x2222222222222222222222222222222222222222"},"Score":9},
			{"ID":"` + zero + `","Bid":{"Id":"` + bid2 + `","Provider":"0x0000000000000000000000000000000000000000"},"Score":8},
			{"ID":"` + zero + `","Bid":{"Id":"","Provider":"0x3333333333333333333333333333333333333333"},"Score":7}]}`))
	})
	mux.HandleFunc("POST /blockchain/bids/{id}/session", func(w http.ResponseWriter, r *http.Request) {
		opened = r.PathValue("id")
		_, _ = w.Write([]byte(`{"sessionID":"0xs1"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	c := New(ts.URL, "u", "p")
	bids, err := c.RatedBids("0xmodel")
	if err != nil {
		t.Fatal(err)
	}
	if len(bids) != 1 || bids[0].ID != bid1 || bids[0].Provider != "0x559b16c3d329f058b5413b0009465e64df66a16b" {
		t.Fatalf("bids %+v", bids)
	}
	if _, err := c.OpenSessionByBid(bids[0].ID, 600); err != nil {
		t.Fatal(err)
	}
	if opened != bid1 {
		t.Fatalf("opened by bid %q, want %q", opened, bid1)
	}
}

func TestZeroHex(t *testing.T) {
	for s, want := range map[string]bool{
		"": true, "0x": true, "0x0": true, "0x0000000000000000000000000000000000000000": true,
		"0X00": true, "0x01": false, "0xabc": false, "abc": false,
	} {
		if got := zeroHex(s); got != want {
			t.Errorf("zeroHex(%q) = %v", s, got)
		}
	}
}
