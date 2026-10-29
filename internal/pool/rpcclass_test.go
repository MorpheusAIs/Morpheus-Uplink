package pool

// Open-error classification (router RPC rate limiting). An open error is
// transient only when a known chain-stage text (testdata/
// router_stage_prefixes.txt) is followed by a rate-limit marker; a
// provider-stage text always cools the provider; any other open error is
// maybe-sent (no second open, reconciled in the request). The
// expectations FAIL on the current code until the fix lands; the guards
// pass today and must keep passing.

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type stageText struct{ class, text string }

func stageTexts(t *testing.T) []stageText {
	t.Helper()
	f, err := os.Open("testdata/router_stage_prefixes.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []stageText
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		class, text, ok := strings.Cut(line, "\t")
		if !ok || text == "" {
			t.Fatalf("fixture line %q: want <class><TAB><text>", line)
		}
		out = append(out, stageText{class, text})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func stageClass(t *testing.T, class string) []string {
	var out []string
	for _, s := range stageTexts(t) {
		if s.class == class {
			out = append(out, s.text)
		}
	}
	return out
}

// routerErr is a router status-500 body.
func routerErr(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b)
}

// Guard: the fixture is well formed and covers the stages seen in the
// incident (rpcSteps, the receipt wait and the rated-bids read).
func TestStageFixture(t *testing.T) {
	known := map[string]bool{"presend": true, "sent": true, "read": true, "provider": true, "marker": true}
	seen := map[string]bool{}
	for _, s := range stageTexts(t) {
		if !known[s.class] {
			t.Errorf("unknown class %q for %q", s.class, s.text)
		}
		if seen[s.text] {
			t.Errorf("duplicate text %q", s.text)
		}
		seen[s.text] = true
	}
	covered := func(body, class string) bool {
		for _, p := range stageClass(t, class) {
			if strings.Contains(body, p) {
				return true
			}
		}
		return false
	}
	for step := range rpcSteps {
		if !covered(rpc429(step), "presend") || !covered(rpc429(step), "marker") {
			t.Errorf("incident shape for %q not covered by a presend text and a marker", step)
		}
	}
	if !covered(receiptWait429, "sent") || !covered(rated429, "read") {
		t.Error("receipt-wait or rated-bids incident shape not covered")
	}
}

// (a) Transient: each pre-send chain stage, bare and inside the router's
// send wrappers, followed by each rate-limit marker. One fault, then the
// same bid is retried after one backoff step; no walk, no cool-down.
func TestTransientIsChainStagePlusMarker(t *testing.T) {
	for _, stage := range stageClass(t, "presend") {
		for _, marker := range stageClass(t, "marker") {
			for _, wrap := range []string{"", "failed to send transaction: open session failed: "} {
				name := strings.ReplaceAll(stage+"|"+marker+"|"+map[bool]string{true: "bare", false: "wrapped"}[wrap == ""], " ", "_")
				t.Run(name, func(t *testing.T) {
					_, lr, p, sr := rpcEnv(t)
					threeBidders(lr)
					lr.setOpenFault(faultBurst(1, routerErr(wrap+stage+": "+marker), false))
					_, res, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
					assertOnlyTopBid(t, lr, modelB)
					if n := len(lr.bidsTried()); n != 2 {
						t.Errorf("open attempts = %d, want 2 (retry the same bid)", n)
					}
					assertBackoff(t, sr.sleeps(), 1)
					assertNoCooling(t, p, modelB)
					if err != nil || !res.Opened {
						t.Errorf("acquire: res %+v err %v", res, err)
					}
				})
			}
		}
	}
}

// (a) Not transient, and (b) maybe-sent: errors with no known stage text
// (bare network texts, a rate-limit text with no chain stage, the
// router's send wrapper alone). The open may have been sent, so: exactly
// one open (no same-bid retry, no walk), reconciled in the request (3
// polls 2s apart), then refused.
func TestUnclassifiedOpenErrorIsMaybeSent(t *testing.T) {
	for _, msg := range []string{
		"timeout",
		"connection refused",
		"EOF",
		"rate limit exceeded",
		"429 Too Many Requests",
		"failed to send transaction: 429 Too Many Requests",
	} {
		t.Run(strings.ReplaceAll(msg, " ", "_"), func(t *testing.T) {
			_, lr, p, sr := rpcEnv(t)
			threeBidders(lr)
			lr.setOpenFault(faultBurst(-1, routerErr(msg), false))
			_, _, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
			if n := len(lr.bidsTried()); n != 1 {
				t.Errorf("opens = %d, want 1 (maybe-sent: never re-opened)", n)
			}
			lr.mu.Lock()
			lists := lr.lists
			lr.mu.Unlock()
			if lists < reconcilePolls {
				t.Errorf("session-list reads = %d, want >= %d (in-request reconcile)", lists, reconcilePolls)
			}
			assertReconcileSleeps(t, sr.sleeps())
			if err == nil {
				t.Error("acquire succeeded with nothing landed")
			}
		})
	}
}

// (b) A maybe-sent open whose tx did land (unclassified error): the
// reconcile adopts the landed session; no second open.
func TestUnclassifiedOpenErrorLandedIsAdopted(t *testing.T) {
	_, lr, p, _ := rpcEnv(t)
	threeBidders(lr)
	lr.setOpenFault(faultBurst(1, routerErr("EOF"), true))
	_, res, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
	if n := len(lr.bidsTried()); n != 1 {
		t.Errorf("opens = %d, want 1 (maybe-sent: never re-opened)", n)
	}
	if err != nil || res.SessionID != "0xs001" {
		t.Errorf("acquire: res %+v err %v, want the landed session 0xs001 adopted", res, err)
	}
	if u := unheld(lr, p); len(u) > 0 {
		t.Errorf("on-chain sessions neither pooled nor closed: %v", u)
	}
}

// Guard (passes today, must keep passing): a provider-stage text always
// cools the provider and walks on, even when it carries a rate-limit or
// network text.
func TestProviderStageAlwaysCools(t *testing.T) {
	for _, stage := range stageClass(t, "provider") {
		for _, tail := range []string{"dial tcp: connection refused", "429 Too Many Requests"} {
			t.Run(strings.ReplaceAll(stage+"|"+tail, " ", "_"), func(t *testing.T) {
				_, lr, p, _ := rpcEnv(t)
				threeBidders(lr)
				lr.setOpenFault(faultBurst(1, routerErr(stage+": "+tail), false))
				_, res, err := p.Acquire(modelB, AcquireOpts{KeyID: "k1"})
				if err != nil || !res.Opened {
					t.Fatalf("acquire: res %+v err %v", res, err)
				}
				if !p.IsCooling(modelB, leaseProv1) {
					t.Error("provider-stage fault did not cool the provider")
				}
				if d := distinct(lr.bidsTried()); len(d) != 2 {
					t.Errorf("bids tried = %d, want 2 (walk past the failed provider)", len(d))
				}
			})
		}
	}
}
