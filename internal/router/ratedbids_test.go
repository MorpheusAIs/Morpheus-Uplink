package router

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	zeroHash = "0x0000000000000000000000000000000000000000000000000000000000000000"
	fakeBid1 = "0xaaaa0000000000000000000000000000000000000000000000000000000000a1"
	fakeBid2 = "0xbbbb0000000000000000000000000000000000000000000000000000000000b2"
	// Mixed case, to check the lowercase result.
	fakeProv1 = "0xAbCd000000000000000000000000000000000001"
	fakeProv2 = "0x2222222222222222222222222222222222222222"
)

func ratedServer(t *testing.T, body string, opened *string) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /blockchain/models/{id}/bids/rated", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("POST /blockchain/bids/{id}/session", func(w http.ResponseWriter, r *http.Request) {
		*opened = r.PathValue("id")
		_, _ = w.Write([]byte(`{"sessionID":"0xs1"}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return New(ts.URL, "u", "p")
}

func row(outer, id, prov string, score int) string {
	return `{"ID":"` + outer + `","Bid":{"Id":"` + id + `","Provider":"` + prov + `"},"Score":` + string(rune('0'+score)) + `}`
}

// The router's rated-bids rows carry the bid id in Bid.Id; the outer ID is
// left unset and comes out as the zero hash. A by-bid open must use Bid.Id.
func TestRatedBidsUsesInnerBidID(t *testing.T) {
	var opened string
	c := ratedServer(t, `{"bids":[`+row(zeroHash, fakeBid1, fakeProv1, 2)+`,`+row(zeroHash, fakeBid2, fakeProv2, 3)+`]}`, &opened)
	bids, err := c.RatedBids("0xmodel")
	if err != nil {
		t.Fatal(err)
	}
	if len(bids) != 2 || bids[0].ID != fakeBid2 || bids[1].ID != fakeBid1 || bids[1].Provider != strings.ToLower(fakeProv1) {
		t.Fatalf("bids %+v", bids)
	}
	if _, err := c.OpenSessionByBid(bids[1].ID, 600); err != nil {
		t.Fatal(err)
	}
	if opened != fakeBid1 {
		t.Fatalf("opened by bid %q, want %q", opened, fakeBid1)
	}
}

// Rows without a valid bid id or provider are dropped and reported: the
// valid rows come back with ErrRatedBidsIncomplete. The outer ID is used
// only when Bid.Id is not valid and the outer ID is.
func TestRatedBidsReportsUnusableRows(t *testing.T) {
	var opened string
	for name, bad := range map[string]string{
		"zero bid id":       row(zeroHash, zeroHash, fakeProv2, 9),
		"empty bid id":      row(zeroHash, "", fakeProv2, 9),
		"short bid id":      row(zeroHash, "0xabc", fakeProv2, 9),
		"non-hex bid id":    row(zeroHash, "0x"+strings.Repeat("g", 64), fakeProv2, 9),
		"zero provider":     row(zeroHash, fakeBid2, "0x0000000000000000000000000000000000000000", 9),
		"provider no 0x":    row(zeroHash, fakeBid2, strings.TrimPrefix(fakeProv2, "0x"), 9),
		"short provider":    row(zeroHash, fakeBid2, "0x2222", 9),
		"non-hex provider":  row(zeroHash, fakeBid2, "0x"+strings.Repeat("z", 40), 9),
		"provider too long": row(zeroHash, fakeBid2, fakeProv2+"00", 9),
	} {
		c := ratedServer(t, `{"bids":[`+row(zeroHash, fakeBid1, fakeProv1, 2)+`,`+bad+`]}`, &opened)
		bids, err := c.RatedBids("0xmodel")
		if !errors.Is(err, ErrRatedBidsIncomplete) || len(bids) != 1 || bids[0].ID != fakeBid1 {
			t.Errorf("%s: bids %+v err %v", name, bids, err)
		}
	}
	c := ratedServer(t, `{"bids":[`+row(fakeBid2, zeroHash, fakeProv2, 2)+`]}`, &opened)
	if bids, err := c.RatedBids("0xmodel"); err != nil || len(bids) != 1 || bids[0].ID != fakeBid2 {
		t.Fatalf("outer fallback: %+v %v", bids, err)
	}
	c = ratedServer(t, `{"bids":[]}`, &opened)
	if bids, err := c.RatedBids("0xmodel"); err != nil || len(bids) != 0 {
		t.Fatalf("empty: %+v %v", bids, err)
	}
}

func TestHexValue(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
		ok   bool
	}{
		{"", 40, "", false},
		{"0x", 40, "", false},
		{"abc", 40, "", false},
		{"0x0000000000000000000000000000000000000000", 40, "", false},
		{" 0X" + strings.Repeat("A", 40) + " ", 40, "0x" + strings.Repeat("a", 40), true},
		{"0x" + strings.Repeat("1", 39), 40, "", false},
		{"0x" + strings.Repeat("1", 41), 40, "", false},
		{"0x" + strings.Repeat("g", 40), 40, "", false},
		{fakeBid1, 64, fakeBid1, true},
		{fakeBid1, 40, "", false},
	} {
		if got, ok := hexValue(c.in, c.n); got != c.want || ok != c.ok {
			t.Errorf("hexValue(%q, %d) = %q %v", c.in, c.n, got, ok)
		}
	}
}
