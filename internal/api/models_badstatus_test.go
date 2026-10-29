package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/absgrafx/morpheus-uplink/internal/config"
)

// TestModelsPriceIgnoresNonStringStatusRow: a bidDetail row whose status is
// not a JSON string (1, true, null, {}, or absent) is dropped from the
// catalog, and a row whose status is a string other than "healthy" ("",
// "degraded") does not count while a healthy bid exists, so a cheaper row
// like that never becomes the price /v1/models shows: the listed price is
// the one the file gives without it.
func TestModelsPriceIgnoresNonStringStatusRow(t *testing.T) {
	prov := "0x" + strings.Repeat("a", 40)
	good := `{"bidId":"0x` + strings.Repeat("b1", 32) + `","providerId":"` + prov + `","status":"healthy","pricePerSecond":"100","priceMorPerHour":1}`
	for _, bad := range []string{`1`, `true`, `null`, `{}`, "absent", `""`, `"degraded"`} {
		t.Run(bad, func(t *testing.T) {
			st := `,"status":` + bad
			if bad == "absent" {
				st = ""
			}
			cheap := `{"bidId":"0x` + strings.Repeat("bf", 32) + `","providerId":"` + prov + `","pricePerSecond":"1","priceMorPerHour":0.0001` + st + `}`
			feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"models":[{"Name":"test-model","Id":"` + testModelID + `","Tags":["chat"],"bidDetail":[` + good + `,` + cheap + `]}]}`))
			}))
			t.Cleanup(feed.Close)
			m := newFakeMarket(t, prov)
			e := newFailoverEnv(t, m, func(c *config.Config) { c.ActiveModelsURL = feed.URL })
			req, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/v1/models", nil)
			req.Header.Set("Authorization", "Bearer "+e.key)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("/v1/models %d %s", resp.StatusCode, raw)
			}
			var list struct {
				Data []struct {
					Morpheus struct {
						BlockchainID      string  `json:"blockchainId"`
						PriceMorPerHour   float64 `json:"priceMorPerHour"`
						PricePerSecondWei string  `json:"pricePerSecondWei"`
					} `json:"morpheus"`
				}
			}
			if err := json.Unmarshal(raw, &list); err != nil {
				t.Fatalf("%v %s", err, raw)
			}
			if len(list.Data) != 1 || list.Data[0].Morpheus.BlockchainID != testModelID {
				t.Fatalf("listed %s", raw)
			}
			if p := list.Data[0].Morpheus; p.PriceMorPerHour != 1 || p.PricePerSecondWei != "100" {
				t.Fatalf("price %+v, want 1 MOR/h and 100 wei/s (the bad row ignored)", p)
			}
		})
	}
}
