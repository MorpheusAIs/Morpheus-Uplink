package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Health feed constants (not settings).
const (
	// staleAfter is when a feed counts as stale (display and log only;
	// staleness never refuses an open).
	staleAfter = 30 * time.Minute
	// staleWarnEvery bounds the repeated stale and rollback log lines.
	staleWarnEvery = 10 * time.Minute
	// maxFeedSkew is how far in the future a feed's last_updated may be.
	maxFeedSkew = 5 * time.Minute
	// maxFeedBytes caps the decoded size of each feed.
	maxFeedBytes = 8 << 20
)

// ErrHealthUnknown means no good copy of either health feed has loaded
// since start.
var ErrHealthUnknown = errors.New("provider health unknown: no health feed has loaded")

// healthSnap is one immutable health copy: model id -> healthy bid ids
// (lowercase). It is replaced whole, never mutated after publish.
type healthSnap struct {
	set         map[string]map[string]struct{}
	lastUpdated int64 // the feed's last_updated (Unix seconds)
	fetchedAt   time.Time
}

// feedLog is the per-feed log rate-limit state.
type feedLog struct {
	staleLogged        bool
	lastStaleWarnAt    time.Time
	lastRollbackLogAt  time.Time
	rollbackSuppressed int
}

// healthState lives under Catalog.mu.
type healthState struct {
	bids        *healthSnap // nil until the first good bids copy
	bidsErr     error
	bidsErrAt   time.Time
	detail      *healthSnap // bidDetail fallback; nil until a good models copy
	detailErr   error
	detailErrAt time.Time
	// modelsLastUpdated is the last_updated of the latest models copy
	// that had a valid one (display only).
	modelsLastUpdated int64
	modelsHasStamp    bool
	logs              map[string]*feedLog
}

// feedError is a failed feed file with its error class.
type feedError struct {
	class string
	err   error
}

func (e *feedError) Error() string {
	if e.err == nil {
		return e.class
	}
	return e.class + ": " + e.err.Error()
}
func (e *feedError) Unwrap() error { return e.err }

func feedErrorClass(err error) string {
	var fe *feedError
	if errors.As(err, &fe) {
		return fe.class
	}
	return "fetch"
}

// noRedirect makes any 3xx the final response (a failed fetch).
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// fetchFeed GETs a feed with the catalog client (default transport: proxy
// from the environment, transparent gzip, system roots). It never follows
// a redirect and reads at most maxFeedBytes of decoded body.
func (c *Catalog) fetchFeed(url string) ([]byte, error) {
	resp, err := c.client.Get(url)
	if err != nil {
		return nil, &feedError{class: "fetch", err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, &feedError{class: "redirect", err: fmt.Errorf("status %d", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &feedError{class: "http_status", err: fmt.Errorf("status %d", resp.StatusCode)}
	}
	if !resp.Uncompressed && resp.ContentLength > maxFeedBytes {
		return nil, &feedError{class: "oversize"}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil {
		return nil, &feedError{class: "fetch", err: err}
	}
	if len(raw) > maxFeedBytes {
		return nil, &feedError{class: "oversize"}
	}
	return raw, nil
}

// hexID returns s lowercased when it is 0x + 64 hex digits, not all zeros.
func hexID(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) != 66 || !strings.HasPrefix(s, "0x") {
		return "", false
	}
	zero := true
	for _, r := range s[2:] {
		switch {
		case r == '0':
		case r >= '1' && r <= '9', r >= 'a' && r <= 'f':
			zero = false
		default:
			return "", false
		}
	}
	if zero {
		return "", false
	}
	return s, true
}

// jsonString returns the value of a JSON string, ok=false for any other
// JSON value (or none).
func jsonString(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func isHealthy(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "healthy")
}

// feedStamp parses last_updated: a JSON integer (Unix seconds) no more
// than maxFeedSkew in the future.
func feedStamp(raw json.RawMessage, now time.Time) (int64, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0, &feedError{class: "bad_timestamp", err: errors.New("last_updated missing")}
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, &feedError{class: "bad_timestamp", err: errors.New("last_updated is not an integer")}
	}
	if time.Unix(n, 0).After(now.Add(maxFeedSkew)) {
		return 0, &feedError{class: "bad_timestamp", err: errors.New("last_updated is in the future")}
	}
	return n, nil
}

// healthCounts are per-refresh counts for the log (never row content).
type healthCounts struct{ rows, usable, healthy, skipped int }

// parseBidsFeed parses the gateway bids file into model -> healthy bids.
// Rows: Id, ModelAgentId and health.status only (the feed's provider is
// never used). A file is good with a valid last_updated, a bids array and
// at least one usable row, healthy or not.
func parseBidsFeed(raw []byte, now time.Time) (*healthSnap, healthCounts, error) {
	var file struct {
		LastUpdated json.RawMessage `json:"last_updated"`
		Bids        json.RawMessage `json:"bids"`
	}
	var cnt healthCounts
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, cnt, &feedError{class: "malformed", err: err}
	}
	stamp, err := feedStamp(file.LastUpdated, now)
	if err != nil {
		return nil, cnt, err
	}
	b := bytes.TrimSpace(file.Bids)
	if len(b) == 0 || b[0] != '[' {
		return nil, cnt, &feedError{class: "malformed", err: errors.New("bids is not an array")}
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, cnt, &feedError{class: "malformed", err: err}
	}
	set := map[string]map[string]struct{}{}
	for _, r := range rows {
		cnt.rows++
		var row struct {
			ID     string `json:"Id"`
			Model  string `json:"ModelAgentId"`
			Health *struct {
				Status json.RawMessage `json:"status"`
			} `json:"health"`
		}
		if json.Unmarshal(r, &row) != nil || row.Health == nil {
			cnt.skipped++
			continue
		}
		id, okID := hexID(row.ID)
		model, okModel := hexID(row.Model)
		status, okStatus := jsonString(row.Health.Status)
		if !okID || !okModel || !okStatus {
			cnt.skipped++
			continue
		}
		cnt.usable++
		if !isHealthy(status) {
			continue
		}
		cnt.healthy++
		if set[model] == nil {
			set[model] = map[string]struct{}{}
		}
		set[model][id] = struct{}{}
	}
	if cnt.usable == 0 {
		return nil, cnt, &feedError{class: "no_usable_rows"}
	}
	return &healthSnap{set: set, lastUpdated: stamp}, cnt, nil
}

// detailParse is the bidDetail health parse of one models copy.
type detailParse struct {
	snap   *healthSnap // nil when the copy cannot be used for health
	err    error
	stamp  int64
	stamps bool // last_updated was valid (for display)
	cnt    healthCounts
}

// parseBidDetail builds the bidDetail health fallback from a models copy:
// a row is usable when its model Id and bidId are 0x + 64 hex (not all
// zeros) and its status is a JSON string; healthy when that status is
// "healthy". The copy needs a valid last_updated and a usable row.
func parseBidDetail(raw []byte, now time.Time) *detailParse {
	var file struct {
		LastUpdated json.RawMessage `json:"last_updated"`
		Models      []struct {
			ID        string `json:"Id"`
			BidDetail []json.RawMessage `json:"bidDetail"`
		} `json:"models"`
	}
	out := &detailParse{}
	if err := json.Unmarshal(raw, &file); err != nil {
		out.err = &feedError{class: "malformed", err: err}
		return out
	}
	stamp, err := feedStamp(file.LastUpdated, now)
	if err != nil {
		out.err = err
		return out
	}
	out.stamp, out.stamps = stamp, true
	set := map[string]map[string]struct{}{}
	for _, m := range file.Models {
		model, okModel := hexID(m.ID)
		for _, r := range m.BidDetail {
			out.cnt.rows++
			var row struct {
				BidID  string          `json:"bidId"`
				Status json.RawMessage `json:"status"`
			}
			if !okModel || json.Unmarshal(r, &row) != nil {
				out.cnt.skipped++
				continue
			}
			id, okID := hexID(row.BidID)
			status, okStatus := jsonString(row.Status)
			if !okID || !okStatus {
				out.cnt.skipped++
				continue
			}
			out.cnt.usable++
			if !isHealthy(status) {
				continue
			}
			out.cnt.healthy++
			if set[model] == nil {
				set[model] = map[string]struct{}{}
			}
			set[model][id] = struct{}{}
		}
	}
	if out.cnt.usable == 0 {
		out.err = &feedError{class: "no_usable_rows"}
		return out
	}
	out.snap = &healthSnap{set: set, lastUpdated: stamp}
	return out
}

// replaceSnapLocked applies the monotonic rule: a newer last_updated
// replaces the copy, an equal one only refreshes its fetch time, an older
// one is a rollback (failed file).
func replaceSnapLocked(cur **healthSnap, next *healthSnap, now time.Time) error {
	if *cur != nil {
		switch {
		case next.lastUpdated < (*cur).lastUpdated:
			return &feedError{class: "rollback", err: fmt.Errorf("last_updated %d older than %d", next.lastUpdated, (*cur).lastUpdated)}
		case next.lastUpdated == (*cur).lastUpdated:
			keep := *(*cur)
			keep.fetchedAt = now
			*cur = &keep
			return nil
		}
	}
	next.fetchedAt = now
	*cur = next
	return nil
}

func (c *Catalog) feedLogLocked(feed string) *feedLog {
	if c.health.logs == nil {
		c.health.logs = map[string]*feedLog{}
	}
	l := c.health.logs[feed]
	if l == nil {
		l = &feedLog{}
		c.health.logs[feed] = l
	}
	return l
}

// failedLocked logs a failed feed file (class only, never the body).
// Repeated rollback lines are rate-limited per feed.
func (c *Catalog) failedLocked(feed, class string, now time.Time) {
	if class != "rollback" {
		slog.Warn("catalog feed failed", "feed", feed, "error", class)
		return
	}
	l := c.feedLogLocked(feed)
	if !l.lastRollbackLogAt.IsZero() && now.Sub(l.lastRollbackLogAt) < staleWarnEvery {
		l.rollbackSuppressed++
		return
	}
	slog.Warn("catalog feed failed", "feed", feed, "error", class, "suppressed", l.rollbackSuppressed)
	l.lastRollbackLogAt, l.rollbackSuppressed = now, 0
}

// detailFailedLocked records a failed models file for the bidDetail
// fallback (error and time; the snapshot is kept, its fetch time is not
// refreshed).
func (c *Catalog) detailFailedLocked(class string, now time.Time) {
	c.health.detailErr, c.health.detailErrAt = &feedError{class: class}, now
	c.failedLocked("models", class, now)
}

// applyDetailLocked publishes the bidDetail health copy of a good models
// fetch. It returns the failure (the caller takes the shared pause).
func (c *Catalog) applyDetailLocked(d *detailParse, now time.Time) error {
	if d == nil {
		return nil
	}
	if d.stamps {
		c.health.modelsLastUpdated, c.health.modelsHasStamp = d.stamp, true
	} else {
		c.health.modelsHasStamp = false
	}
	err := d.err
	if err == nil {
		err = replaceSnapLocked(&c.health.detail, d.snap, now)
	}
	if err != nil {
		c.detailFailedLocked(feedErrorClass(err), now)
		return err
	}
	c.health.detailErr = nil
	slog.Info("catalog feed refreshed", "feed", "models", "rows", d.cnt.rows, "usable", d.cnt.usable, "healthy", d.cnt.healthy, "skipped", d.cnt.skipped, "age_s", int64(now.Sub(time.Unix(d.stamp, 0)).Seconds()))
	return nil
}

// bidsState reports whether the bids feed needs a fetch now (copy older
// than the TTL and its failure pause over) and whether health is known.
func (c *Catalog) bidsState() (due bool, known bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := c.now()
	known = c.health.bids != nil || c.health.detail != nil
	if c.bidsURL == "" {
		return false, known
	}
	if c.health.bids != nil && now.Sub(c.health.bids.fetchedAt) < c.ttl {
		return false, known
	}
	if c.health.bidsErr != nil && now.Sub(c.health.bidsErrAt) < c.retryAfter {
		return false, known
	}
	return true, known
}

// refreshBids refreshes the bids feed like refreshModels: never under mu,
// serialized by bidsFetchMu; a caller with any health copy does not wait
// for a running fetch.
func (c *Catalog) refreshBids() {
	due, known := c.bidsState()
	if !due {
		return
	}
	if !c.bidsFetchMu.TryLock() {
		if known {
			return
		}
		c.bidsFetchMu.Lock()
	}
	defer c.bidsFetchMu.Unlock()
	if due, _ := c.bidsState(); !due {
		return
	}
	raw, err := c.fetchFeed(c.bidsURL)
	now := c.now()
	var snap *healthSnap
	var cnt healthCounts
	if err == nil {
		snap, cnt, err = parseBidsFeed(raw, now)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		err = replaceSnapLocked(&c.health.bids, snap, now)
	}
	if err != nil {
		c.health.bidsErr, c.health.bidsErrAt = err, now
		c.failedLocked("bids", feedErrorClass(err), now)
		return
	}
	c.health.bidsErr = nil
	slog.Info("catalog feed refreshed", "feed", "bids", "rows", cnt.rows, "usable", cnt.usable, "healthy", cnt.healthy, "skipped", cnt.skipped, "age_s", int64(now.Sub(time.Unix(c.health.bids.lastUpdated, 0)).Seconds()))
}

// HealthyBidIDs returns the model's healthy bid ids (lowercase, a copy):
//  1. once the bids file has loaded a good copy, that copy, however old,
//     even when it has no healthy bid for the model (final);
//  2. else the latest good models-file bidDetail copy, however old;
//  3. else ErrHealthUnknown.
func (c *Catalog) HealthyBidIDs(modelID string) (map[string]struct{}, error) {
	_ = c.ensureFresh()
	model := strings.ToLower(strings.TrimSpace(modelID))
	c.mu.RLock()
	defer c.mu.RUnlock()
	snap := c.health.bids
	if snap == nil {
		snap = c.health.detail
	}
	if snap == nil {
		return nil, ErrHealthUnknown
	}
	out := make(map[string]struct{}, len(snap.set[model]))
	for id := range snap.set[model] {
		out[id] = struct{}{}
	}
	return out, nil
}

// FeedState is one feed's age for the admin Status page. Times are RFC3339
// UTC; nil when never loaded.
type FeedState struct {
	LastGoodFetchAt *string `json:"last_good_fetch_at"`
	FeedLastUpdated *string `json:"feed_last_updated"`
	AgeSeconds      *int64  `json:"age_seconds"`
	Stale           bool    `json:"stale"`
	Loaded          bool    `json:"loaded"`
}

func feedState(fetched time.Time, stamp int64, hasStamp bool, now time.Time) FeedState {
	if fetched.IsZero() {
		return FeedState{}
	}
	st := FeedState{Loaded: true}
	f := fetched.UTC().Format(time.RFC3339)
	st.LastGoodFetchAt = &f
	oldest := fetched
	if hasStamp {
		lu := time.Unix(stamp, 0)
		u := lu.UTC().Format(time.RFC3339)
		st.FeedLastUpdated = &u
		if lu.Before(oldest) {
			oldest = lu
		}
	}
	age := int64(now.Sub(oldest) / time.Second)
	if age < 0 {
		age = 0
	}
	st.AgeSeconds = &age
	st.Stale = now.Sub(oldest) > staleAfter
	return st
}

// healthSourceLocked names the current health source.
func (c *Catalog) healthSourceLocked() string {
	switch {
	case c.health.bids != nil:
		return "bids"
	case c.health.detail != nil:
		return "bid_detail"
	}
	return "none"
}

func (c *Catalog) feedStatesLocked(now time.Time) (models, bids FeedState, source string) {
	source = c.healthSourceLocked()
	if source == "bid_detail" {
		d := c.health.detail
		models = feedState(d.fetchedAt, d.lastUpdated, true, now)
	} else if c.models != nil {
		models = feedState(c.fetchedAt, c.health.modelsLastUpdated, c.health.modelsHasStamp, now)
	}
	if b := c.health.bids; b != nil {
		bids = feedState(b.fetchedAt, b.lastUpdated, true, now)
	}
	return models, bids, source
}

// FeedStatus reports both feeds' age and the health source. It reads
// state only and never fetches (the Status page polls it).
func (c *Catalog) FeedStatus() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	models, bids, source := c.feedStatesLocked(c.now())
	return map[string]any{"models": models, "bids": bids, "health_source": source}
}

// checkStale logs a warning when a loaded feed turns stale, then at most
// every staleWarnEvery while it stays stale, and one line on recovery.
func (c *Catalog) checkStale() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	models, bids, _ := c.feedStatesLocked(now)
	for _, f := range []struct {
		name string
		st   FeedState
	}{{"models", models}, {"bids", bids}} {
		l := c.feedLogLocked(f.name)
		if !f.st.Stale {
			if l.staleLogged {
				slog.Info("catalog feed fresh again", "feed", f.name)
				l.staleLogged = false
			}
			continue
		}
		if l.staleLogged && now.Sub(l.lastStaleWarnAt) < staleWarnEvery {
			continue
		}
		slog.Warn("catalog feed stale", "feed", f.name, "age_seconds", *f.st.AgeSeconds, "last_good_fetch_at", *f.st.LastGoodFetchAt, "feed_last_updated", deref(f.st.FeedLastUpdated))
		l.staleLogged, l.lastStaleWarnAt = true, now
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
