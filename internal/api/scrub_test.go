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
// body); statuses are unchanged; the cap is rune-safe; the full text goes
// to the ops log only.
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
		secret []string // must reach the log, never the client
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
		{"big openai-shaped", 429, `{"error":{"message":"slow down ` + addr + `","type":"rate_limit","pad":"` + strings.Repeat("x", 6000) + `"}}`, []string{addr}},
		{"untouched", 429, `{"error":"rate limited, retry later"}`, nil},
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
		if len(out) > maxRelayBody+1024 {
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
			if !strings.Contains(logged, s) {
				t.Errorf("%s: %q missing from the ops log", tc.name, s)
			}
		}
		if tc.name == "untouched" && msg != "rate limited, retry later" {
			t.Errorf("plain text changed: %q", msg)
		}
	}
	if got := truncate("ab"+"é", 3); got != "ab…" || !utf8.ValidString(got) {
		t.Errorf("truncate split a rune: %q", got)
	}
}
