// Package catalog maps human model names to blockchain model IDs using the
// active.mor.org gateway catalog JSON (gateway_models.json by default).
// List/open pricing uses each model's bidDetail from that file (primary).
// GATEWAY_BIDS_URL (gateway_bids.json) is the companion bid feed for
// mirrors/ops; it is not merged into LowestBid (different schema).
package catalog

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

type BidDetail struct {
	BidID           string  `json:"bidId"`
	ProviderID      string  `json:"providerId"`
	PricePerSecond  string  `json:"pricePerSecond"`
	PriceMorPerHour float64 `json:"priceMorPerHour"`
	Status          string  `json:"status"`
}

type Model struct {
	Name      string      `json:"Name"`
	ID        string      `json:"Id"`
	Tags      []string    `json:"Tags"`
	ModelType string      `json:"ModelType"`
	BidDetail []BidDetail `json:"bidDetail"`
}

// LowestBid returns the cheapest healthy bid (by pricePerSecond), or any
// positive bid if none are marked healthy. ok=false when no usable bid.
func (m Model) LowestBid() (BidDetail, bool) {
	var best BidDetail
	var bestPPS *big.Int
	pick := func(onlyHealthy bool) {
		for _, b := range m.BidDetail {
			if onlyHealthy {
				if b.Status != "" && !strings.EqualFold(b.Status, "healthy") {
					continue
				}
			}
			pps, ok := new(big.Int).SetString(strings.TrimSpace(b.PricePerSecond), 10)
			if !ok || pps.Sign() <= 0 {
				continue
			}
			if bestPPS == nil || pps.Cmp(bestPPS) < 0 {
				bestPPS = pps
				best = b
			}
		}
	}
	pick(true)
	if bestPPS == nil {
		pick(false)
	}
	return best, bestPPS != nil
}

// LowestMorPerHour is the cheapest bid's catalog MOR/hour (display only).
func (m Model) LowestMorPerHour() float64 {
	b, ok := m.LowestBid()
	if !ok {
		return 0
	}
	return b.PriceMorPerHour
}

// LowestPricePerSecondWei is the cheapest bid's price/second in wei.
func (m Model) LowestPricePerSecondWei() *big.Int {
	b, ok := m.LowestBid()
	if !ok {
		return nil
	}
	n, ok := new(big.Int).SetString(strings.TrimSpace(b.PricePerSecond), 10)
	if !ok {
		return nil
	}
	return n
}

type activeModelsFile struct {
	Models []Model `json:"models"`
}

type Catalog struct {
	url     string // ACTIVE_MODELS_URL / gateway_models (bidDetail primary for list/open)
	bidsURL string // GATEWAY_BIDS_URL companion feed; not merged into LowestBid
	client  *http.Client

	// fetchMu serializes feed fetches; it is never held with mu, and mu is
	// never held during a fetch.
	fetchMu sync.Mutex

	mu        sync.Mutex
	models    []Model
	byName    map[string]string // lowercase name -> id
	byID      map[string]string // lowercase id -> display name
	fetchedAt time.Time
	ttl       time.Duration
	// After a failed fetch no new fetch starts for retryAfter: callers get
	// the stale data, or lastErr while nothing has ever loaded. This keeps
	// repeated lookups (status polls, unknown ids) from becoming a fetch
	// per call while the feed is down.
	retryAfter time.Duration
	lastErr    error
	lastErrAt  time.Time
}

// DefaultRetryAfter is the pause after a failed catalog fetch.
const DefaultRetryAfter = 10 * time.Second

func New(url string) *Catalog {
	return &Catalog{
		url:        url,
		client:     &http.Client{Timeout: 15 * time.Second},
		ttl:        5 * time.Minute,
		retryAfter: DefaultRetryAfter,
	}
}

// SetRetryAfter sets the pause after a failed fetch (0: none).
func (c *Catalog) SetRetryAfter(d time.Duration) {
	c.mu.Lock()
	c.retryAfter = d
	c.mu.Unlock()
}

// Loaded reports whether the catalog has data (possibly stale). It never
// fetches.
func (c *Catalog) Loaded() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.models != nil
}

// WithBidsURL records the companion gateway_bids feed URL (GATEWAY_BIDS_URL).
// Catalog refresh still uses model bidDetail only; see package comment.
func (c *Catalog) WithBidsURL(bidsURL string) *Catalog {
	c.bidsURL = bidsURL
	return c
}

// GatewayBidsURL returns the companion bids feed URL, if set.
func (c *Catalog) GatewayBidsURL() string { return c.bidsURL }

// fetch reads and indexes the feed. It touches no Catalog state, so it
// runs without mu.
func (c *Catalog) fetch() ([]Model, map[string]string, map[string]string, error) {
	resp, err := c.client.Get(c.url)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("fetch model catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, nil, fmt.Errorf("fetch model catalog: status %d", resp.StatusCode)
	}
	var file activeModelsFile
	if err := json.NewDecoder(resp.Body).Decode(&file); err != nil {
		return nil, nil, nil, fmt.Errorf("parse model catalog: %w", err)
	}
	byName, byID := indexModels(file.Models)
	return file.Models, byName, byID, nil
}

// state reports, under mu, whether the data is fresh and, when a fetch
// should not start now (pause after a failure), the error to return.
func (c *Catalog) state() (fresh bool, wait bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.models != nil && time.Since(c.fetchedAt) < c.ttl {
		return true, false, nil
	}
	if c.lastErr != nil && time.Since(c.lastErrAt) < c.retryAfter {
		if c.models != nil {
			return false, true, nil // serve stale
		}
		return false, true, c.lastErr
	}
	return false, false, nil
}

// ensureFresh refreshes stale data. It must be called WITHOUT mu: the
// network fetch never runs under mu. Fetches are serialized (fetchMu);
// while one runs, callers with stale data get it at once and callers with
// no data wait for its result instead of starting their own.
func (c *Catalog) ensureFresh() error {
	if fresh, wait, err := c.state(); fresh || wait {
		return err
	}
	if !c.fetchMu.TryLock() {
		if c.Loaded() {
			return nil // a fetch is running; serve stale
		}
		c.fetchMu.Lock()
	}
	defer c.fetchMu.Unlock()
	// Another caller may have fetched (or failed) while we waited.
	if fresh, wait, err := c.state(); fresh || wait {
		return err
	}
	models, byName, byID, err := c.fetch()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.lastErr, c.lastErrAt = err, time.Now()
		// Serve stale data over failing hard if we have any.
		if c.models != nil {
			return nil
		}
		return err
	}
	c.models, c.byName, c.byID = models, byName, byID
	c.fetchedAt = time.Now()
	c.lastErr = nil
	return nil
}

// Resolve maps a model name (or an already-resolved 0x id) to a blockchain
// model ID. When several catalog rows share a name, the winner is the
// cheapest row that has a healthy bid. Rows with no healthy bid lose to
// rows that have one. Equal prices break ties by lowercase model ID.
func (c *Catalog) Resolve(nameOrID string) (string, error) {
	if strings.HasPrefix(nameOrID, "0x") {
		return nameOrID, nil
	}
	if err := c.ensureFresh(); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if id, ok := c.byName[strings.ToLower(nameOrID)]; ok {
		return id, nil
	}
	return "", fmt.Errorf("model %q not found in catalog", nameOrID)
}

func (c *Catalog) List() ([]Model, error) {
	if err := c.ensureFresh(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Model, len(c.models))
	copy(out, c.models)
	return out, nil
}

// NameByID returns the catalog display name for a blockchain model id.
// Empty string if unknown.
func (c *Catalog) NameByID(id string) string {
	if err := c.ensureFresh(); err != nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byID[strings.ToLower(id)]
}

// SendableID is the model string clients should send. sharedName is true
// when more than one catalog row uses this display name.
func (m Model) SendableID(sharedName bool) string {
	if sharedName && strings.TrimSpace(m.ID) != "" {
		return m.ID
	}
	return m.Name
}

// indexModels maps lowercase names to one blockchain ID. Duplicate names
// keep the preferred twin (see preferModel) instead of whichever row the
// feed listed last.
func indexModels(models []Model) (byName, byID map[string]string) {
	byName = make(map[string]string, len(models))
	byID = make(map[string]string, len(models))
	chosen := make(map[string]Model, len(models))
	for _, m := range models {
		byID[strings.ToLower(m.ID)] = m.Name
		key := strings.ToLower(m.Name)
		prev, ok := chosen[key]
		if !ok || preferModel(m, prev) {
			chosen[key] = m
		}
	}
	for key, m := range chosen {
		byName[key] = m.ID
	}
	return byName, byID
}

// preferModel reports whether cand should replace cur for a shared name.
func preferModel(cand, cur Model) bool {
	cHealthy, cOK := cand.healthyBidWei()
	uHealthy, uOK := cur.healthyBidWei()
	if cOK != uOK {
		return cOK
	}
	if cOK && uOK {
		if cmp := cHealthy.Cmp(uHealthy); cmp != 0 {
			return cmp < 0
		}
	} else {
		cPrice := cand.LowestPricePerSecondWei()
		uPrice := cur.LowestPricePerSecondWei()
		cHas := cPrice != nil && cPrice.Sign() > 0
		uHas := uPrice != nil && uPrice.Sign() > 0
		if cHas != uHas {
			return cHas
		}
		if cHas && uHas {
			if cmp := cPrice.Cmp(uPrice); cmp != 0 {
				return cmp < 0
			}
		}
	}
	return strings.ToLower(cand.ID) < strings.ToLower(cur.ID)
}

// healthyBidWei is the cheapest bid whose status is healthy or unset.
func (m Model) healthyBidWei() (*big.Int, bool) {
	var best *big.Int
	for _, b := range m.BidDetail {
		if b.Status != "" && !strings.EqualFold(b.Status, "healthy") {
			continue
		}
		n, ok := bidWei(b)
		if !ok {
			continue
		}
		if best == nil || n.Cmp(best) < 0 {
			best = n
		}
	}
	return best, best != nil
}

func bidWei(b BidDetail) (*big.Int, bool) {
	n, ok := new(big.Int).SetString(strings.TrimSpace(b.PricePerSecond), 10)
	if !ok || n.Sign() <= 0 {
		return nil, false
	}
	return n, true
}
