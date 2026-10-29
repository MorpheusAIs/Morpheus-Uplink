package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/catalog"
	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

// Fake ids only: repeated-byte hex and loopback test servers.
const (
	// testProv is the provider of the single-provider mocks.
	testProv = "0xa0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0"
	// testBid is testProv's bid on testModelID.
	testBid = "0xb0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"
)

// feedRow is one bid in the mock gateway feeds and router rated list.
// status "" leaves the health status absent.
type feedRow struct{ model, bid, prov, status string }

// mixedCase writes an address the way the router does (EIP-55 style mixed
// case): Uplink must match it case-insensitively.
func mixedCase(addr string) string {
	b := []byte(addr)
	for i := 2; i < len(b); i++ {
		if i%2 == 0 && b[i] >= 'a' && b[i] <= 'f' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

func utc(ts int64) string { return time.Unix(ts, 0).UTC().Format(time.RFC3339) }

// bidsFeed is a gateway bids file in the real shape.
func bidsFeed(lastUpdated int64, rows ...feedRow) []byte {
	bids := []map[string]any{}
	for _, r := range rows {
		h := map[string]any{"errorKind": "", "httpStatus": 200, "latencyMs": 120, "promptCorrect": true,
			"providerVersion": "test", "providerPingMs": 20, "lastHealthy": lastUpdated, "lastHealthyUTC": utc(lastUpdated),
			"lastChecked": lastUpdated, "lastCheckedUTC": utc(lastUpdated)}
		if r.status != "" {
			h["status"] = r.status
		}
		bids = append(bids, map[string]any{"Id": r.bid, "Provider": r.prov, "ModelAgentId": r.model,
			"PricePerSecond": "1000", "Nonce": "0", "CreatedAt": "1700000000", "DeletedAt": "0",
			"ModelName": "test-model", "health": h})
	}
	raw, _ := json.Marshal(map[string]any{"last_updated": lastUpdated, "last_updated_utc": utc(lastUpdated), "bids": bids})
	return raw
}

// modelsFeed is a gateway models file in the real shape: one model per
// distinct model id (named name), with its rows as bidDetail.
func modelsFeed(lastUpdated int64, name string, rows ...feedRow) []byte {
	type model struct {
		Name      string           `json:"Name"`
		ID        string           `json:"Id"`
		Tags      []string         `json:"Tags"`
		BidDetail []map[string]any `json:"bidDetail"`
	}
	var models []*model
	byID := map[string]*model{}
	for _, r := range rows {
		m := byID[r.model]
		if m == nil {
			m = &model{Name: name, ID: r.model, Tags: []string{"chat"}, BidDetail: []map[string]any{}}
			byID[r.model] = m
			models = append(models, m)
		}
		d := map[string]any{"bidId": r.bid, "providerId": r.prov, "pricePerSecond": "1000", "priceMorPerHour": 0.0036}
		if r.status != "" {
			d["status"] = r.status
		}
		m.BidDetail = append(m.BidDetail, d)
	}
	raw, _ := json.Marshal(map[string]any{"last_updated": lastUpdated, "last_updated_utc": utc(lastUpdated), "models": models})
	return raw
}

// ratedRows is a router rated-bids answer in the real shape (zero outer
// ID, full Bid, mixed-case provider), Score descending in row order.
func ratedRows(rows ...feedRow) []byte {
	bids := []map[string]any{}
	for i, r := range rows {
		bids = append(bids, map[string]any{"ID": "0x" + strings.Repeat("0", 64), "Bid": map[string]any{
			"Id": r.bid, "Provider": mixedCase(r.prov), "ModelAgentId": r.model, "PricePerSecond": "1000",
			"Nonce": "0", "CreatedAt": "1700000000", "DeletedAt": "0"}, "Score": float64(10 - i)})
	}
	raw, _ := json.Marshal(map[string]any{"bids": bids})
	return raw
}

// testRow is the single healthy bid of the single-provider mocks.
var testRow = feedRow{model: testModelID, bid: testBid, prov: testProv, status: "healthy"}

// countByModel registers the by-model open, which Uplink never uses: it
// counts, answers 500, and the test fails at cleanup if it was called.
func countByModel(t *testing.T, mux *http.ServeMux) {
	var n atomic.Int32
	mux.HandleFunc("POST /blockchain/models/{id}/session", func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		http.Error(w, `{"error":"by-model open not expected"}`, http.StatusInternalServerError)
	})
	t.Cleanup(func() {
		if c := n.Load(); c != 0 {
			t.Errorf("by-model opens = %d, want 0", c)
		}
	})
}

// wiredServer builds the Server the way cmd/uplink does: the catalog
// (models feed, and the bids feed when set) is the pool's health source.
func wiredServer(cfg *config.Config, st *store.Store, rc *router.Client, durationSec int) *Server {
	cat := catalog.New(cfg.ActiveModelsURL)
	if cfg.GatewayBidsURL != "" {
		cat = cat.WithBidsURL(cfg.GatewayBidsURL)
	}
	pl := pool.New(rc, durationSec)
	pl.SetHealthyBids(cat.HealthyBidIDs)
	return NewServer(cfg, st, cat, pl, rc, nil)
}
