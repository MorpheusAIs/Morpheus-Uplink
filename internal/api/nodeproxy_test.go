package api

import (
	"bufio"
	"bytes"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/absgrafx/morpheus-uplink/internal/catalog"
	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

// upstream records every request the router stand-in receives.
type upstream struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (u *upstream) count() int { u.mu.Lock(); defer u.mu.Unlock(); return len(u.reqs) }
func (u *upstream) last() *http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.reqs[len(u.reqs)-1]
}

func nodeTestServer(t *testing.T) (*httptest.Server, *Server, *config.Config, *upstream) {
	t.Helper()
	up := &upstream{}
	rt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.mu.Lock()
		up.reqs = append(up.reqs, r.Clone(r.Context()))
		up.mu.Unlock()
		switch {
		case r.URL.Path == "/redirect":
			http.Redirect(w, r, "/blockchain/send/eth", http.StatusFound)
		case r.URL.Path == "/swagger/doc.json":
			_, _ = w.Write([]byte(`{"swagger":"2.0","basePath": "/","paths":{}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(rt.Close)
	cat := mockCatalog(t)
	t.Cleanup(cat.Close)
	cfg := &config.Config{
		RouterURL: rt.URL, RouterUser: "admin", RouterPass: "routerpass",
		AdminPassword: "adminpass", APIKeySeed: "test-seed", DataDir: t.TempDir(),
		ActiveModelsURL: cat.URL, SessionDurationSec: 600, DevHTTP: true,
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	rc := router.New(cfg.RouterURL, cfg.RouterUser, cfg.RouterPass)
	srv := NewServer(cfg, st, catalog.New(cfg.ActiveModelsURL), pool.New(rc, 600, true), rc, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	u, _ := url.Parse(ts.URL)
	cfg.WebPublicURL = u.Scheme + "://" + u.Host
	return ts, srv, cfg, up
}

// rawDo sends the request line byte-for-byte (no client-side path cleaning
// or re-encoding) with the admin session and CSRF headers.
func rawDo(t *testing.T, ts *httptest.Server, c *http.Client, cfg *config.Config, method, rawPath string, hdr map[string]string) int {
	t.Helper()
	u, _ := url.Parse(ts.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var cookies []string
	for _, ck := range c.Jar.Cookies(u) {
		cookies = append(cookies, ck.Name+"="+ck.Value)
	}
	body := `{"to":"0x0000000000000000000000000000000000000001","amount":"1","note":"bodymarker-should-not-be-logged"}`
	var b strings.Builder
	b.WriteString(method + " " + rawPath + " HTTP/1.1\r\nHost: " + u.Host + "\r\nConnection: close\r\n")
	b.WriteString("Cookie: " + strings.Join(cookies, "; ") + "\r\n")
	b.WriteString("Origin: " + cfg.WebPublicURL + "\r\nSec-Fetch-Site: same-origin\r\n")
	b.WriteString("Content-Type: application/json\r\nContent-Length: " + itoa(len(body)) + "\r\n")
	for k, v := range hdr {
		b.WriteString(k + ": " + v + "\r\n")
	}
	b.WriteString("\r\n" + body)
	if _, err := conn.Write([]byte(b.String())); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawPath, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}

func blockAudits(srv *Server) []AuditEvent {
	var out []AuditEvent
	for _, e := range srv.audit.Recent(10000) {
		if e.Kind == "node_passthrough_block" {
			out = append(out, e)
		}
	}
	return out
}

var nodeMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

// Every blocked router route (see nodeproxy.go) plus spelling variants.
var nodeBlockedPaths = []string{
	"/blockchain/send/eth", "/blockchain/send/mor", "/blockchain/send", "/blockchain/send/anything/else",
	"/blockchain/approve",
	"/wallet", "/wallet/privateKey", "/wallet/mnemonic", "/wallet/derivationPath",
	"/auth/users", "/auth/users/request", "/auth/users/confirm", "/auth/users/agent1/txs",
	"/auth/allowance/requests", "/auth/allowance/confirm", "/auth/allowance/revoke", "/auth/cookie/path", "/auth",
	"/v1/audio/transcriptions", "/V1/Audio/Transcriptions/", "/v1/audio/tr%61nscriptions",
	"/docker/build", "/docker/build/stream", "/Docker/Container/Start/",
	"/ipfs/add", "/ipfs/pin", "/ipfs/unpin", "/ipfs/download/stream/QmX", "/ipfs/download/QmX", "/ipfs/version",
	"/blockchain/sessions", "/blockchain/bids/0xabc/session", "/blockchain/models/0xabc/session",
	// case, trailing slash, encoded letters
	"/BLOCKCHAIN/SEND/ETH", "/Blockchain/Send/Mor", "/blockchain/send/eth/", "/WALLET/", "/Auth/Users/",
	"/blockchain/s%65nd/eth", "/blockchain/%73end/mor", "/w%61llet/privateKey", "/blockchain/appr%6Fve",
	"/blockchain/Sessions/", "/blockchain/MODELS/0xabc/Session",
	// ambiguous: encoded separators, encoded dots, double encoding, dot segments, empty segments
	"/blockchain%2fsend/eth", "/blockchain%2Fsend%2Feth", "/blockchain%5csend/eth", "/blockchain\\send/eth",
	"/blockchain/send%252feth", "/blockchain/%252e%252e/send/eth", "/blockchain/%2e%2e/blockchain/send/eth",
	"/%2e/blockchain/send/eth", "/blockchain/./send/eth", "/healthcheck/../blockchain/send/eth",
	"/./wallet", "/../wallet", "//blockchain/send/eth", "/blockchain//send/eth", "/wallet//",
	"/blockchain/send/eth%00", "/blockchain/send/eth%0a",
	// ambiguous even for allowed routes (never decoded twice)
	"/healthcheck%2f", "/blockchain/providers/./", "/blockchain/%2e%2e/providers",
}

func TestNodeBlockedPathsAllMethods(t *testing.T) {
	ts, srv, cfg, up := nodeTestServer(t)
	ac := adminClient(t, ts, cfg)
	n := 0
	for _, p := range nodeBlockedPaths {
		for _, m := range nodeMethods {
			code := rawDo(t, ts, ac, cfg, m, "/node"+p, nil)
			n++
			if code != http.StatusForbidden {
				t.Errorf("%s /node%s: %d, want 403", m, p, code)
			}
			if got := len(blockAudits(srv)); got != n {
				t.Fatalf("%s /node%s: %d block audit events, want %d", m, p, got, n)
			}
		}
	}
	if c := up.count(); c != 0 {
		t.Fatalf("router received %d requests for blocked paths (last %s %s)", c, up.last().Method, up.last().URL.Path)
	}
	for _, e := range blockAudits(srv) {
		if strings.Contains(e.Message, "bodymarker") || strings.Contains(e.Message, "routerpass") || strings.Contains(e.Message, "adminpass") {
			t.Fatalf("audit message carries body or secret: %q", e.Message)
		}
	}
	seen := map[string]bool{}
	for _, e := range blockAudits(srv) {
		ok := false
		for _, m := range nodeMethods {
			if strings.HasPrefix(e.Message, "node_passthrough_block denied "+m+" /node/") {
				ok, seen[m] = true, true
			}
		}
		if !ok {
			t.Fatalf("audit message %q lacks method + path", e.Message)
		}
	}
	if len(seen) != len(nodeMethods) {
		t.Fatalf("audit methods seen: %v", seen)
	}
}

// Malformed escapes never reach the passthrough: Go's server answers 400.
func TestNodeMalformedEscapeRejected(t *testing.T) {
	ts, _, cfg, up := nodeTestServer(t)
	ac := adminClient(t, ts, cfg)
	for _, p := range []string{"/node/blockchain/send/eth%", "/node/blockchain/send/eth%zz", "/node/wallet%2"} {
		if code := rawDo(t, ts, ac, cfg, "POST", p, nil); code != http.StatusBadRequest && code != http.StatusForbidden {
			t.Fatalf("%s: %d, want 400 or 403", p, code)
		}
	}
	if up.count() != 0 {
		t.Fatal("malformed path reached the router")
	}
}

// A multipart audio transcription carrying file_path and s3_presigned_url
// is refused before anything is forwarded.
func TestNodeAudioTranscriptionMultipartBlocked(t *testing.T) {
	ts, srv, cfg, up := nodeTestServer(t)
	ac := adminClient(t, ts, cfg)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", "whisper")
	_ = mw.WriteField("file_path", "/data/sample.wav")
	_ = mw.WriteField("s3_presigned_url", "https://bucket.example/sample.wav")
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/node/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	withCSRF(req, cfg)
	resp, err := ac.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("multipart transcription: %d, want 403", resp.StatusCode)
	}
	ev := blockAudits(srv)
	if len(ev) != 1 || !strings.Contains(ev[0].Message, "POST /node/v1/audio/transcriptions (blocked)") {
		t.Fatalf("audit: %+v", ev)
	}
	if strings.Contains(ev[0].Message, "sample.wav") {
		t.Fatalf("audit carries a form field: %q", ev[0].Message)
	}
	if up.count() != 0 {
		t.Fatal("transcription request reached the router")
	}
}

func TestNodeBlockAuditNormalizedPath(t *testing.T) {
	ts, srv, cfg, _ := nodeTestServer(t)
	ac := adminClient(t, ts, cfg)
	rawDo(t, ts, ac, cfg, "POST", "/node/blockchain/s%65nd/eth/", nil)
	ev := blockAudits(srv)
	if len(ev) != 1 || !strings.Contains(ev[0].Message, "POST /node/blockchain/send/eth (blocked)") {
		t.Fatalf("audit: %+v", ev)
	}
}

func TestNodeAllowedPathsForwardNormalized(t *testing.T) {
	ts, srv, cfg, up := nodeTestServer(t)
	ac := adminClient(t, ts, cfg)
	cases := []struct{ method, raw, want string }{
		{"GET", "/node/healthcheck", "/healthcheck"},
		{"GET", "/node/blockchain/sessions/user", "/blockchain/sessions/user"},
		{"GET", "/node/blockchain/sessions/0xabc", "/blockchain/sessions/0xabc"},
		{"POST", "/node/blockchain/sessions/0xabc/close", "/blockchain/sessions/0xabc/close"},
		{"GET", "/node/blockchain/providers/", "/blockchain/providers"},
		{"GET", "/node/blockchain/provid%65rs", "/blockchain/providers"},
		{"GET", "/node/blockchain/models", "/blockchain/models"},
		{"GET", "/node/blockchain/models/0xabc/session/estimate", "/blockchain/models/0xabc/session/estimate"},
		{"GET", "/node/blockchain/sessions/budget", "/blockchain/sessions/budget"},
	}
	for _, c := range cases {
		before := up.count()
		code := rawDo(t, ts, ac, cfg, c.method, c.raw+"?_method=DELETE&_METHOD=POST&keep=1",
			map[string]string{"X-HTTP-Method-Override": "DELETE", "X-HTTP-Method": "DELETE", "X-Method-Override": "DELETE"})
		if code != http.StatusOK {
			t.Fatalf("%s %s: %d, want 200", c.method, c.raw, code)
		}
		if up.count() != before+1 {
			t.Fatalf("%s %s: not forwarded", c.method, c.raw)
		}
		r := up.last()
		if r.Method != c.method || r.URL.Path != c.want || r.URL.RawPath != "" {
			t.Fatalf("%s %s: router saw %s %q (raw %q), want %s %q", c.method, c.raw, r.Method, r.URL.Path, r.URL.RawPath, c.method, c.want)
		}
		q := r.URL.Query()
		if q.Get("keep") != "1" || len(q) != 1 {
			t.Fatalf("%s: forwarded query %q, want only keep=1", c.raw, r.URL.RawQuery)
		}
		for _, h := range []string{"X-Http-Method-Override", "X-Http-Method", "X-Method-Override", "Cookie", "Origin", "Sec-Fetch-Site"} {
			if r.Header.Get(h) != "" {
				t.Fatalf("%s: header %s forwarded", c.raw, h)
			}
		}
		if u, p, ok := r.BasicAuth(); !ok || u != "admin" || p != "routerpass" {
			t.Fatalf("%s: router credential not injected", c.raw)
		}
	}
	if n := len(blockAudits(srv)); n != 0 {
		t.Fatalf("allowed paths wrote %d block audits", n)
	}
}

func TestNodeRequiresAdmin(t *testing.T) {
	ts, _, _, up := nodeTestServer(t)
	for _, p := range []string{"/node/healthcheck", "/node/blockchain/send/eth", "/node/../node/healthcheck"} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s: %d, want 401", p, resp.StatusCode)
		}
	}
	if up.count() != 0 {
		t.Fatal("unauthenticated request reached the router")
	}
}

func TestNodeRedirectNotFollowed(t *testing.T) {
	ts, _, cfg, up := nodeTestServer(t)
	ac := adminClient(t, ts, cfg)
	code := rawDo(t, ts, ac, cfg, "GET", "/node/redirect", nil)
	if code != http.StatusBadGateway {
		t.Fatalf("router redirect: %d, want 502", code)
	}
	if up.count() != 1 || up.last().URL.Path != "/redirect" {
		t.Fatalf("redirect was chased: %d upstream requests", up.count())
	}
}

func TestNodeSwaggerBasePathRewriteKept(t *testing.T) {
	ts, _, cfg, _ := nodeTestServer(t)
	ac := adminClient(t, ts, cfg)
	resp, err := ac.Get(ts.URL + "/node/swagger/doc.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := make([]byte, 512)
	n, _ := resp.Body.Read(b)
	if !strings.Contains(string(b[:n]), `"basePath": "/node"`) {
		t.Fatalf("swagger basePath not rewritten: %s", b[:n])
	}
}

func TestNormalizeNodePath(t *testing.T) {
	ok := map[string]string{"/": "/", "/healthcheck/": "/healthcheck", "/a%62c": "/abc", "/Blockchain/Models": "/Blockchain/Models"}
	for in, want := range ok {
		if got, err := normalizeNodePath(in); err != nil || got != want {
			t.Errorf("normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "x", "//", "/a//b", "/a/./b", "/a/../b", "/..", "/a%2fb", "/a%2Fb", "/a%5cb", "/a%252fb", "/a%2eb", "/a%25", "/a%", "/a%g1", "/a\\b", "/a%00", "/a\x7f"} {
		if got, err := normalizeNodePath(in); err == nil {
			t.Errorf("normalize(%q) = %q, want ambiguous", in, got)
		}
	}
}

func TestAdminNoStoreHeaders(t *testing.T) {
	ts, _, cfg, _ := nodeTestServer(t)
	ac := adminClient(t, ts, cfg)
	check := func(resp *http.Response, what string) {
		t.Helper()
		resp.Body.Close()
		if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Pragma") != "no-cache" {
			t.Fatalf("%s: Cache-Control=%q Pragma=%q", what, resp.Header.Get("Cache-Control"), resp.Header.Get("Pragma"))
		}
	}
	resp, err := ac.Get(ts.URL + "/admin/keys?reveal=1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reveal: %d", resp.StatusCode)
	}
	check(resp, "GET /admin/keys?reveal=1")
	resp, err = http.Get(ts.URL + "/admin/status") // unauthenticated error response
	if err != nil {
		t.Fatal(err)
	}
	check(resp, "401 /admin/status")
}
