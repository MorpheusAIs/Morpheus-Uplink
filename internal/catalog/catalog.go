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
	// excluded: the providerExclude setting (lowercase 0x addresses). Their
	// bids are dropped from List and from the shared-name choice; a model
	// left with no bid is hidden (see withoutExcluded).
	excluded map[string]bool
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

// SetProviderExclude installs the configured provider exclusions (for
// every model) and re-indexes any loaded data.
func (c *Catalog) SetProviderExclude(provs []string) {
	ex := map[string]bool{}
	for _, pr := range provs {
		if pr = strings.ToLower(strings.TrimSpace(pr)); pr != "" {
			ex[pr] = true
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.excluded = ex
	if c.models != nil {
		c.byName, c.byID = c.indexLocked(c.models)
	}
}

// withoutExcluded drops the bids of excluded providers. A model whose
// every bid is excluded is left out; a model the feed lists with no bids
// at all is kept as it is. The input is not modified.
func withoutExcluded(models []Model, excluded map[string]bool) []Model {
	out := make([]Model, 0, len(models))
	for _, m := range models {
		if len(excluded) == 0 || len(m.BidDetail) == 0 {
			out = append(out, m)
			continue
		}
		bids := make([]BidDetail, 0, len(m.BidDetail))
		for _, b := range m.BidDetail {
			if !excluded[strings.ToLower(strings.TrimSpace(b.ProviderID))] {
				bids = append(bids, b)
			}
		}
		if len(bids) == 0 {
			continue
		}
		m.BidDetail = bids
		out = append(out, m)
	}
	return out
}

// indexLocked builds the name and id indexes (mu held). The shared-name
// choice only sees bids outside the exclusions; a name whose every row is
// hidden still resolves (to its feed choice) so a request for it gets the
// pool's no-eligible-provider answer rather than "not found". byID keeps
// every feed row so names stay known for labels.
func (c *Catalog) indexLocked(models []Model) (byName, byID map[string]string) {
	byName, byID = indexModels(models)
	if len(c.excluded) == 0 {
		return byName, byID
	}
	visible, _ := indexModels(withoutExcluded(models, c.excluded))
	for k, id := range visible {
		byName[k] = id
	}
	return byName, byID
}

// WithBidsURL records the companion gateway_bids feed URL (GATEWAY_BIDS_URL).
// Catalog refresh still uses model bidDetail only; see package comment.
func (c *Catalog) WithBidsURL(bidsURL string) *Catalog {
	c.bidsURL = bidsURL
	return c
}

// GatewayBidsURL returns the companion bids feed URL, if set.
func (c *Catalog) GatewayBidsURL() string { return c.bidsURL }

// fetch reads the feed. It touches no Catalog state, so it runs without
// mu.
func (c *Catalog) fetch() ([]Model, error) {
	resp, err := c.client.Get(c.url)
	if err != nil {
		return nil, fmt.Errorf("fetch model catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch model catalog: status %d", resp.StatusCode)
	}
	var file activeModelsFile
	if err := json.NewDecoder(resp.Body).Decode(&file); err != nil {
		return nil, fmt.Errorf("parse model catalog: %w", err)
	}
	return file.Models, nil
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
	models, err := c.fetch()
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
	c.models = models
	c.byName, c.byID = c.indexLocked(models)
	c.fetchedAt = time.Now()
	c.lastErr = nil
	return nil
}

// Resolve maps a model name (or an already-resolved 0x id) to a blockchain
// model ID. When several catalog rows share a name, the winner is the
// cheapest row that has a healthy bid. Rows with no healthy bid lose to
// rows that have one. Equal prices break ties by lowercase model ID.
// Bids of excluded providers are not considered (see indexLocked).
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

// List is the catalog as clients see it: bids of excluded providers are
// dropped, so prices and stake estimates ignore them, and a model left
// with no bid is not listed.
func (c *Catalog) List() ([]Model, error) {
	if err := c.ensureFresh(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return withoutExcluded(c.models, c.excluded), nil
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
