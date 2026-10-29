package pool

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// Transient chain-RPC errors from the router, and the bound on stake an
// open may lock while its outcome is unknown.
//
// The router answers a failed open with HTTP 500 {"error": "<stage>: ..."}.
// Only a router chain-stage prefix that ends before any open transaction
// is broadcast, followed by the router's own rate-limit wording, is
// retried (same bid, backoff). Provider-stage texts always cool the
// provider. Anything else on the open may have been sent: it is
// reconciled against the wallet's session list, never re-opened.
//
// Every value is a constant: there is no setting for any of them.
const (
	transientBase       = 500 * time.Millisecond // first backoff
	transientCap        = 4 * time.Second        // per-step cap; also the router_transient Retry-After cap
	transientMaxRetries = 4                      // per Acquire / per failover reopen, across all calls
	transientBudget     = 15 * time.Second       // no new retry starts after this (from the operation start)
	reconcilePolls      = 3                      // maybe-sent: list polls inside the request
	reconcileGap        = 2 * time.Second        // between those polls
	attemptSkew         = 30 * time.Second       // match window slack, both ends
	routerOpenMaxLife   = 13 * time.Minute       // upper bound on a router open call's life after Uplink stops waiting
	routerCloseMaxLife  = 10 * time.Minute       // the same for a router close call
	attemptKeep         = routerOpenMaxLife      // unresolved attempts kept in memory
	maxPendingAttempts  = 32                     // bound on that list; the oldest is dropped with a warning
	maxWindowRetries    = 1                      // at most one P* (18 s rule) retry per open operation
	preSendProofWindow  = 18 * time.Second       // below the router's 20 s wait after a send

	orphanScanEvery       = 2 * time.Minute  // periodic sweep scan
	closesPerHold         = 1                // sweep and boot closes per slot hold, then release
	sharedWalletWarnEvery = 10 * time.Minute // log rate limit
	foreignAdoptWarn      = 3                // adoptions of never-opened sessions per hour
	sweepMaxSkips         = 3                // skipped sweep ticks before the sweep queues for one close
	orphanAlertAge        = 10 * time.Minute // oldest close-due age that forces a queued close and alerts
	orphanAlertCount      = 3                // close-due ids that trigger the orphan alert
)

// openClass is what an open error says about the open.
type openClass int

const (
	// maybeSent: an open transaction may be on-chain. Reconcile; never re-open.
	maybeSent openClass = iota
	// providerFault: the provider (or the bid) failed: cool it and walk.
	providerFault
	// transientPreSend: a rate limit before any open transaction: back off, same bid.
	transientPreSend
	// transientWindow: a P* rate limit proven pre-send only by the 18 s
	// rule: back off on the same bid at most maxWindowRetries times.
	transientWindow
)

func (c openClass) String() string {
	switch c {
	case providerFault:
		return "provider_fault"
	case transientPreSend:
		return "transient_presend"
	case transientWindow:
		return "transient_window"
	}
	return "maybe_sent"
}

// Router v7.18.0 stage texts (lowercase). A fixture in testdata pins them.
var (
	// providerStageTexts match anywhere: the provider failed, whatever else
	// the text says.
	providerStageTexts = []string{
		"provider healthcheck ping failed",
		"provider request failed",
		"failed to connect to provider",
		"provider self-reports model",
		"failed to initiate session",
		"tee attestation failed",
		"cannot open session with own bid",
	}
	// preSendPrefixes start a body for a stage that ends before any open
	// transaction is broadcast (the approve tx locks no stake).
	preSendPrefixes = []string{
		"failed to parse token supply: ",
		"failed to parse token budget: ",
		"failed to get bid: ",
		"failed to get my address: ",
		"failed to get provider: ",
		"failed to get current mor allowance: ",
		"failed to send transaction: approve mor allowance failed: ",
	}
	// windowPrefixes (P*) are pre-send only when the open POST failed in
	// under preSendProofWindow, with one router RPC URL.
	windowPrefixes = []string{
		"failed to get transactopts: ",
		"failed to send transaction: open session failed: failed to get initial gas prices: ",
		"failed to send transaction: open session failed: failed to get nonce: ",
		"failed to send transaction: open session failed: failed to build transaction: ",
	}
	// rateLimitMarkers are the router's own rate-limit words (its RPC retry
	// filter) plus the incident's "compute units per second". 429 is a
	// whole token (rateLimit429).
	rateLimitMarkers = []string{
		"too many requests",
		"rate limit",
		"usage limit",
		"quota exceeded",
		"request limit",
		"daily limit",
		"monthly limit",
		"compute units per second",
	}
	rateLimit429 = regexp.MustCompile(`\b429\b`)
	// readPrefixes start a read error body from the chain (session list,
	// rated bids).
	readPrefixes = []string{"failed to call contract"}
)

// hasRateLimitMarker reports whether rest carries a rate-limit marker and
// no "revert" (the router never retries a revert either).
func hasRateLimitMarker(rest string) bool {
	if strings.Contains(rest, "revert") {
		return false
	}
	if rateLimit429.MatchString(rest) {
		return true
	}
	for _, m := range rateLimitMarkers {
		if strings.Contains(rest, m) {
			return true
		}
	}
	return false
}

// routerErrorText is the router's lowercase "error" text of a status error
// (the raw body when it is not that JSON shape).
func routerErrorText(se *router.StatusError) string {
	body := strings.TrimSpace(se.Body)
	var v struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(body), &v) == nil && v.Error != "" {
		body = v.Error
	}
	return strings.ToLower(strings.TrimSpace(body))
}

// isTransportErr: the reply did not come from the router itself (a Go
// transport error, our own deadline, or a 502/503/504 from a proxy).
func isTransportErr(err error) bool {
	var se *router.StatusError
	if errors.As(err, &se) {
		return se.Code == http.StatusBadGateway || se.Code == http.StatusServiceUnavailable || se.Code == http.StatusGatewayTimeout
	}
	var ue *url.Error
	var ne net.Error
	return errors.As(err, &ue) || errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// classifyOpenErr classifies an open-by-bid error. postElapsed is how long
// the open POST ran; windowOK is false when the 18 s rule may not be used
// (router RPC URL count not 1 or unknown, or the operation's one window
// retry is spent). Order: transport first, then the provider-stage deny
// list, then anchored pre-send prefixes plus a marker; everything else is
// maybe-sent.
func classifyOpenErr(err error, postElapsed time.Duration, windowOK bool) openClass {
	if err == nil {
		return maybeSent
	}
	if isTransportErr(err) {
		return maybeSent
	}
	var se *router.StatusError
	if !errors.As(err, &se) {
		// An unreadable 200 answer, or anything else that is not the
		// router's own error reply.
		return maybeSent
	}
	text := routerErrorText(se)
	for _, p := range providerStageTexts {
		if strings.Contains(text, p) {
			return providerFault
		}
	}
	for _, p := range preSendPrefixes {
		if strings.HasPrefix(text, p) {
			if hasRateLimitMarker(text[len(p):]) {
				return transientPreSend
			}
			return providerFault
		}
	}
	for _, p := range windowPrefixes {
		if strings.HasPrefix(text, p) {
			if !windowOK || postElapsed >= preSendProofWindow {
				return maybeSent
			}
			if hasRateLimitMarker(text[len(p):]) {
				return transientWindow
			}
			return providerFault
		}
	}
	return maybeSent
}

// isTransientRead reports whether a read error (rated bids, session list,
// wallet) is worth a backoff: a transport error or 502/503/504,
// a 429 status, or the router's contract-call prefix (or a bare body)
// carrying a rate-limit marker.
func isTransientRead(err error) bool {
	if err == nil {
		return false
	}
	if isTransportErr(err) {
		return true
	}
	var se *router.StatusError
	if !errors.As(err, &se) {
		return false
	}
	if se.Code == http.StatusTooManyRequests {
		return true
	}
	text := routerErrorText(se)
	for _, p := range readPrefixes {
		if strings.HasPrefix(text, p) {
			return hasRateLimitMarker(text[len(p):])
		}
	}
	return strings.HasPrefix(text, "429 ") || strings.HasPrefix(text, "too many requests")
}

// backoffStep is the nominal delay before retry n (0-based): transientBase
// doubled n times, capped at transientCap.
func backoffStep(n int) time.Duration {
	d := transientBase
	for i := 0; i < n && d < transientCap; i++ {
		d *= 2
	}
	if d > transientCap {
		d = transientCap
	}
	return d
}

// jittered is equal jitter: d/2 + rand[0, d/2).
func jittered(d time.Duration) time.Duration {
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(rand.Int64N(int64(half)))
}

// Refusal reasons carried by *NoAvailableError.
const (
	ReasonRouterTransient = "router_transient" // Retry-After 1–4 s
	ReasonOpenBusy        = "open_busy"        // no Retry-After
)

// errTransient is the router_transient refusal. retryIn is clamped to
// [1 s, transientCap] and always set.
func errTransient(retryIn time.Duration) error {
	return &NoAvailableError{RetryAfter: clampTransient(retryIn), Reason: ReasonRouterTransient}
}

// errOpenBusyRefusal is the open_busy refusal (no Retry-After).
func errOpenBusyRefusal() error {
	return &NoAvailableError{Reason: ReasonOpenBusy}
}

func clampTransient(d time.Duration) time.Duration {
	sec := (d + time.Second - 1) / time.Second
	if sec < 1 {
		sec = 1
	}
	if c := transientCap / time.Second; sec > c {
		sec = c
	}
	return sec * time.Second
}
