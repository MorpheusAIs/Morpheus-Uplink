package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// Security gate SHOULD-1: router error text is scrubbed before the cap on
// every relay path (message, already OpenAI-shaped body, providerModelError
// body); statuses are unchanged; the cap is rune-safe; the text goes to
// the ops log only, with URLs and 32-byte ids masked (router.Redact).
func TestRelayRouterErrorScrubsRouterText(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&buf, &mu}, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	addr := "0x" + strings.Repeat("ab", 20)
	id64 := "0x" + strings.Repeat("cd", 32)
	sig := "0x" + strings.Repeat("ef", 65)
	j := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	long := strings.Repeat("é", 400) + " dial tcp 10.1.2.3:8443 " + strings.Repeat("界", 400) // scrub must come before the cap
	cases := []struct {
		name   string
		code   int
		body   string
		secret []string // must reach the log (masked by router.Redact), never the client
	}{
		{"dial host", 500, `{"error":"failed to connect to provider: dial tcp provider-7.example.net:3333: i/o timeout"}`, []string{"provider-7.example.net"}},
		{"dial ipv4", 502, `{"error":"failed to connect to provider: dial tcp 203.0.113.9:3333: connection refused"}`, []string{"203.0.113.9"}},
		{"dial ipv6", 502, `{"error":"dial tcp [2001:db8::1]:3333: connection refused"}`, []string{"2001:db8::1"}},
		{"bare ipv6", 502, `{"error":"no route to fe80:0:0:0:1:2:3:4"}`, []string{"fe80:0:0:0:1:2:3:4"}},
		{"model id", 404, `{"error":"model not found: ` + id64 + `"}`, []string{id64}},
		{"bid and model", 400, `{"error":"bid ` + id64 + ` is not among the rated bids for model ` + id64 + `"}`, []string{id64}},
		{"provider address", 500, `{"error":"code at provider ` + addr + `: owner() on ` + addr + `"}`, []string{addr}},
		{"signature", 500, `{"error":"EVM error: transfer ` + sig + `"}`, []string{sig}},
		{"url", 502, `{"error":"get url https://explorer.example.org/api?x=1 failed"}`, []string{"https://explorer.example.org"}},
		{"localhost", 503, `{"error":"provider did not respond: localhost:8082"}`, []string{"localhost:8082"}},
		{"trace", 500, `{"error":"panic: runtime error\ngoroutine 7 [running]:\nmain.go:42"}`, []string{"goroutine 7"}},
		{"plain text body", 502, "provider error: dial 198.51.100.7:443 at internal.corp.example", []string{"198.51.100.7", "internal.corp.example"}},
		{"openai-shaped", 422, `{"error":{"message":"bad input from ` + addr + `","type":"invalid_request_error","param":"http://10.0.0.5:8080/x"}}`, []string{addr, "10.0.0.5"}},
		{"provider model error", 400, `{"providerModelError":{"error":{"message":"upstream https://llm.internal.example/v1 said no","host":"10.9.8.7:11434"},"owner":"` + addr + `"},"statusCode":400}`, []string{"llm.internal.example", "10.9.8.7", addr}},
		{"long multibyte", 500, `{"error":` + j(long) + `}`, []string{"10.1.2.3"}},
		{"oversized providerModelError", 400, `{"providerModelError":{"error":{"message":"bad input at 10.9.8.7 ` + strings.Repeat("m", 3900) + `"}}}`, []string{"10.9.8.7"}},
		{"providerModelError of escaped bytes", 400, `{"providerModelError":{"error":{"message":"` + strings.Repeat("<", 3000) + ` 10.9.8.6"}}}`, []string{"10.9.8.6"}},
		{"big openai-shaped", 429, `{"error":{"message":"slow down ` + addr + `","type":"rate_limit","pad":"` + strings.Repeat("x", 6000) + `"}}`, []string{addr}},
		{"untouched", 429, `{"error":"rate limited, retry later"}`, nil},
		{"ipv6 bracketed with port", 502, `{"error":"dial tcp [2001:db8:85a3::8a2e:370:7334]:443: refused"}`, []string{"2001:db8:85a3::8a2e:370:7334"}},
		{"ipv6 zone id", 502, `{"error":"dial tcp [fe80::1ff:fe23:4567:890a%eth0]:3333 and fe80::abcd%eth1 down"}`, []string{"fe80::1ff:fe23:4567:890a", "fe80::abcd", "eth0", "eth1"}},
		{"ipv6 compressed", 502, `{"error":"route to 2001:db8::7 and ::1 failed"}`, []string{"2001:db8::7", "::1"}},
		{"ipv6 full 8 groups", 502, `{"error":"peer 2001:0db8:0000:0000:0000:ff00:0042:8329 reset"}`, []string{"2001:0db8:0000:0000:0000:ff00:0042:8329"}},
		{"ipv4-mapped ipv6", 502, `{"error":"peer ::ffff:192.0.2.128 reset"}`, []string{"::ffff:192.0.2.128", "192.0.2.128"}},
		{"clock time kept", 503, `{"error":"provider busy until 15:20:00 or 12:34:56, retry later"}`, nil},
	}
	bad := regexp.MustCompile(`(?i)0x[0-9a-f]{40,}|://|\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b|\[[0-9a-f:]*:[0-9a-f:]*\]|goroutine`)
	for _, tc := range cases {
		mu.Lock()
		buf.Reset()
		mu.Unlock()
		rec := httptest.NewRecorder()
		relayRouterError(rec, &router.StatusError{Method: "POST", Path: "/v1/chat/completions", Code: tc.code, Body: tc.body})
		out := rec.Body.String()
		if rec.Code != tc.code {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, tc.code)
		}
		if bad.MatchString(out) {
			t.Errorf("%s: router detail reached the client: %s", tc.name, out)
		}
		if len(out) > maxRelayBody {
			t.Errorf("%s: body %d bytes", tc.name, len(out))
		}
		var parsed struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
			t.Errorf("%s: body not JSON: %s", tc.name, out)
			continue
		}
		msg := parsed.Error.Message
		if len(strings.TrimSuffix(msg, "…")) > 1000 || !utf8.ValidString(msg) || msg == "" {
			t.Errorf("%s: message %d bytes, valid=%v", tc.name, len(msg), utf8.ValidString(msg))
		}
		mu.Lock()
		logged := buf.String()
		mu.Unlock()
		for _, s := range tc.secret {
			if strings.Contains(out, s) {
				t.Errorf("%s: %q reached the client", tc.name, s)
			}
			// Text inside a URL is masked with the URL; the rest is kept.
			if inLog := strings.Contains(router.Redact(tc.body), router.Redact(s)); inLog != strings.Contains(logged, router.Redact(s)) {
				t.Errorf("%s: %q in the ops log = %v, want %v", tc.name, router.Redact(s), !inLog, inLog)
			}
		}
		if strings.Contains(logged, "://") || strings.Contains(logged, id64) {
			t.Errorf("%s: unmasked URL or id in the ops log: %s", tc.name, logged)
		}
		if tc.name == "untouched" && msg != "rate limited, retry later" {
			t.Errorf("plain text changed: %q", msg)
		}
		if tc.name == "clock time kept" && (!strings.Contains(msg, "15:20:00") || !strings.Contains(msg, "12:34:56")) {
			t.Errorf("clock time scrubbed: %q", msg)
		}
	}
	if got := truncate("ab"+"é", 3); got != "ab…" || !utf8.ValidString(got) {
		t.Errorf("truncate split a rune: %q", got)
	}
}

// The IP rules: bracketed and bare IPv6 (zone, IPv4 tail, port) run before
// IPv4 and each address becomes exactly one [ip]; clock times and
// timestamps stay as they are.
func TestScrubRouterTextIPRows(t *testing.T) {
	for _, in := range []string{
		"[2001:db8::1]:3333", "[::1]:8080", "[fe80::1%eth0]:3333", "fe80::1%eth0", "2001:db8::1", "::1",
		"2001:db8::", "2001:0db8:0000:0000:0000:ff00:0042:8329", "::ffff:10.0.0.5", "[::ffff:10.0.0.5]:443",
		"10.0.0.5", "10.0.0.5:8082",
	} {
		if got := scrubRouterText("at " + in + " now"); got != "at [ip] now" {
			t.Errorf("%q -> %q, want one [ip]", in, got)
		}
	}
	for _, keep := range []string{"15:20:00 ET", "2026-10-27T15:20:00Z", "03:04", "1:2:3", "12:34:56"} {
		if got := scrubRouterText("at " + keep + " now"); got != "at "+keep+" now" {
			t.Errorf("%q changed to %q", keep, got)
		}
	}
}

// The whole reshaped body fits maxRelayBody: an oversized providerModelError
// becomes the minimal {error:{message}} form, and that fits too.
func TestRelayRouterErrorWholeBodyCap(t *testing.T) {
	for _, msg := range []string{strings.Repeat("m", 3900), strings.Repeat("<", 4000), strings.Repeat("界", 1300)} {
		body := `{"providerModelError":{"error":{"message":"` + msg + `"},"detail":"x"}}`
		rec := httptest.NewRecorder()
		relayRouterError(rec, &router.StatusError{Method: "POST", Path: "/v1/chat/completions", Code: 400, Body: body})
		if n := rec.Body.Len(); n > maxRelayBody || n > 4096 {
			t.Fatalf("full body %d bytes, want <= 4096", n)
		}
		var got struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			PME map[string]json.RawMessage `json:"providerModelError"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		if got.Error.Message == "" || !utf8.ValidString(got.Error.Message) {
			t.Fatalf("message %q", got.Error.Message)
		}
		if len(got.PME) != 1 || got.PME["error"] == nil {
			t.Fatalf("providerModelError not the minimal form: %v", got.PME)
		}
	}
	// A small providerModelError is kept whole.
	rec := httptest.NewRecorder()
	relayRouterError(rec, &router.StatusError{Code: 400, Body: `{"providerModelError":{"error":{"message":"bad"},"detail":"x"}}`})
	if !strings.Contains(rec.Body.String(), `"detail":"x"`) {
		t.Fatalf("small providerModelError not kept: %s", rec.Body.String())
	}
}
