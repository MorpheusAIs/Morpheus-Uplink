package api

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/catalog"
	"github.com/absgrafx/morpheus-uplink/internal/config"
	"github.com/absgrafx/morpheus-uplink/internal/keymaker"
	"github.com/absgrafx/morpheus-uplink/internal/pool"
	"github.com/absgrafx/morpheus-uplink/internal/router"
	"github.com/absgrafx/morpheus-uplink/internal/store"
)

// readStream returns every line of a stream (rotated files first).
func readStream(t *testing.T, dir, name string) []AuditEvent {
	t.Helper()
	var out []AuditEvent
	for _, p := range []string{name + ".2", name + ".1", name} {
		f, err := os.Open(filepath.Join(dir, p))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var ev AuditEvent
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				f.Close()
				t.Fatalf("%s: not JSON: %q", p, sc.Text())
			}
			out = append(out, ev)
		}
		f.Close()
	}
	return out
}

func rawFiles(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "audit") {
			d, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			b.Write(d)
		}
	}
	return b.String()
}

func hasKind(evs []AuditEvent, kind, result string) bool {
	for _, e := range evs {
		if e.Kind == kind && (result == "" || e.Result == result) {
			return true
		}
	}
	return false
}

// S2-1: high-value events land in audit-security.jsonl, failures in
// audit.jsonl, both 0600 in DATA_DIR.
func TestS2TwoStreamsRouteByEvent(t *testing.T) {
	ts, srv, cfg, _ := s1Server(t, nil)
	login(t, ts, "wrong-password-x", "")
	ac := adminClient(t, ts, cfg)
	if got := httpCode(t, ac, ts, "/admin/keys?reveal=1"); got != http.StatusOK {
		t.Fatalf("reveal: %d", got)
	}
	code, body := postJSON(t, ac, ts, cfg, "/admin/keys", `{"name":"agent-s2"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created struct {
		Record struct {
			ID string `json:"id"`
		} `json:"record"`
	}
	_ = json.Unmarshal(body, &created)
	if c, _ := s1Do(t, ac, ts, cfg, http.MethodPost, "/admin/pool/close-all", "", nil); c != http.StatusOK {
		t.Fatalf("close-all: %d", c)
	}
	srv.FlushAudit()
	sec := readStream(t, cfg.DataDir, AuditFileSecurity)
	ops := readStream(t, cfg.DataDir, AuditFileOps)
	for _, k := range []string{"login", "key_reveal", "key_create", "pool_close_all"} {
		if !hasKind(sec, k, "ok") {
			t.Errorf("%s ok missing from %s", k, AuditFileSecurity)
		}
		if hasKind(ops, k, "ok") {
			t.Errorf("%s ok in %s", k, AuditFileOps)
		}
	}
	if !hasKind(ops, "login", "fail") || hasKind(sec, "login", "fail") {
		t.Error("login fail must be in audit.jsonl only")
	}
	for _, e := range sec {
		if e.Stream != AuditStreamHighValue {
			t.Fatalf("security line with stream %q", e.Stream)
		}
	}
	for _, name := range []string{AuditFileSecurity, AuditFileOps} {
		fi, err := os.Lstat(filepath.Join(cfg.DataDir, name))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v mode %v", name, err, fi.Mode().Perm())
		}
	}
}

// S2-1: the enumerated routing table.
func TestS2StreamTable(t *testing.T) {
	hv := [][2]string{
		{"login", "ok"}, {"step_up", "ok"}, {"key_reveal", "ok"}, {"key_create", "ok"},
		{"key_import", "ok"}, {"key_revoke", "ok"}, {"2fa_enroll", "ok"}, {"2fa_remove", "ok"},
		{"2fa_reset", "ok"}, {"recovery_codes_regenerated", "ok"}, {"setting_change", "ok"},
		{"pool_close", "ok"}, {"pool_close_all", "ok"},
		{"admin_session_ip_change", "ok"}, {"admin_key_use", "ok"}, {"admin_key_use", "blocked"},
	}
	ops := [][2]string{
		{"admin_bearer_reject", "fail"}, {"admin_bearer_reject", "blocked"}, {"login", "fail"},
		{"login_lockout", "blocked"}, {"step_up", "fail"}, {"session_release", "ok"},
		{"session_release", "fail"}, {"node_passthrough_block", "denied"},
		{"provider_failover", "ok"}, {"session_reconcile", "ok"},
	}
	for _, c := range hv {
		if got := AuditStreamFor(c[0], c[1]); got != AuditStreamHighValue {
			t.Errorf("%v → %s, want security", c, got)
		}
	}
	for _, c := range ops {
		if got := AuditStreamFor(c[0], c[1]); got != AuditStreamOps {
			t.Errorf("%v → %s, want ops", c, got)
		}
	}
}

// S2-1: boot loads the last lines of both streams into the ring; GET
// /admin/audit keeps its shape.
func TestS2BootLoadsBothStreams(t *testing.T) {
	ts, srv, cfg, _ := s1Server(t, nil)
	login(t, ts, "wrong-password-x", "")
	ac := adminClient(t, ts, cfg)
	if httpCode(t, ac, ts, "/admin/keys?reveal=1") != http.StatusOK {
		t.Fatal("reveal")
	}
	srv.FlushAudit()
	ts.Close()

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	rc := router.New(cfg.RouterURL, cfg.RouterUser, cfg.RouterPass)
	srv2 := NewServer(cfg, st, catalog.New(cfg.ActiveModelsURL), pool.New(rc, cfg.SessionDurationSec), rc, nil)
	evs := srv2.audit.Recent(500)
	if !hasKind(evs, "key_reveal", "ok") || !hasKind(evs, "login", "fail") || !hasKind(evs, "login", "ok") {
		t.Fatalf("boot ring missing events: %+v", evs)
	}
	ts2 := httptest.NewServer(srv2.Handler())
	defer ts2.Close()
	u := ts2.URL
	cfg.WebPublicURL = u
	c2 := adminClient(t, ts2, cfg)
	resp, err := c2.Get(u + "/admin/audit")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string][]map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out["events"]) == 0 {
		t.Fatal("no events")
	}
	for _, f := range []string{"at", "kind", "message"} {
		if _, ok := out["events"][0][f]; !ok {
			t.Fatalf("event shape lost %q: %v", f, out["events"][0])
		}
	}
}

// S2-2: the ring keeps at least half of an answer for high-value events.
func TestS2RingReservesHighValueShare(t *testing.T) {
	a := NewAuditLog(500)
	for i := 0; i < 10; i++ {
		a.Add(true, AuditEvent{Kind: "key_reveal", Message: fmt.Sprint(i)})
	}
	for i := 0; i < 5000; i++ {
		a.Add(false, AuditEvent{Kind: "login", Result: "fail"})
	}
	if n := countAudit(a.Recent(500), "key_reveal"); n != 10 {
		t.Fatalf("key_reveal after ops flood: %d, want 10", n)
	}
	for i := 0; i < 1000; i++ {
		a.Add(true, AuditEvent{Kind: "key_create"})
	}
	got := a.Recent(500)
	if len(got) != 500 || countAudit(got, "key_create") < 250 || countAudit(got, "login") < 1 {
		t.Fatalf("shares: total %d hv %d ops %d", len(got), countAudit(got, "key_create"), countAudit(got, "login"))
	}
	if g := a.Recent(51); countAudit(g, "key_create") < 26 {
		t.Fatalf("Recent(51): hv %d, want >= 26", countAudit(g, "key_create"))
	}
}

// S2-2: failure events are aggregated per (IP, event) and per (key id,
// event) per minute, one line with a count.
func TestS2OpsAggregation(t *testing.T) {
	_, srv, cfg, clk := s1Server(t, nil)
	srv.opsAgg.now = clk.Now
	for i := 0; i < 50; i++ {
		srv.auditEvent("admin_bearer_reject", "192.0.2.1", "fail", "path=/admin/keys")
		srv.auditAgent("session_release", "k_agent01", "192.0.2.2", "ok", "released")
	}
	clk.Advance(time.Minute)
	srv.FlushAudit()
	ops := readStream(t, cfg.DataDir, AuditFileOps)
	var rej, rel []AuditEvent
	for _, e := range ops {
		switch e.Kind {
		case "admin_bearer_reject":
			rej = append(rej, e)
		case "session_release":
			rel = append(rel, e)
		}
	}
	if len(rej) != 2 || rej[1].Count != 49 || len(rel) != 2 || rel[1].Count != 49 || rel[0].Actor != "k_agent01" {
		t.Fatalf("aggregation: reject %+v release %+v", rej, rel)
	}
}

// S2-3: the flood. 1M rejected Bearers from rotating addresses and 1M
// release events from one key, on a mock clock over several minutes. The
// last login and key_reveal survive in the security file and in GET
// /admin/audit, audit.jsonl stays within its rotation cap, and the
// aggregation map never passes its bound.
func TestS2AuditFlood(t *testing.T) {
	if testing.Short() {
		t.Skip("flood")
	}
	ts, srv, cfg, clk := s1Server(t, func(c *config.Config) { c.MasterKeyAdmin, c.SessionRelease = true, true })
	srv.opsAgg.now = clk.Now
	srv.auditAgg.now = clk.Now
	srv.lockout.SetClock(clk.Now)
	ac := adminClient(t, ts, cfg)
	if httpCode(t, ac, ts, "/admin/keys?reveal=1") != http.StatusOK {
		t.Fatal("reveal")
	}
	h := srv.Handler()
	floodKey, rec, err := keymaker.NewKey("flood")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddKey(rec); err != nil {
		t.Fatal(err)
	}
	const minutes = 5
	per := s2FloodN / minutes
	bound := opsAggMaxKeys + 8 // keyed buckets + one "other" per event
	maxSeen := 0
	for i := 0; i < s2FloodN; i++ {
		if i > 0 && i%per == 0 {
			clk.Advance(time.Minute + time.Second)
		}
		req := httptest.NewRequest(http.MethodGet, "/admin/status", nil)
		req.Host = ts.Listener.Addr().String() // the console host (WEB_PUBLIC_URL)
		req.RemoteAddr = fmt.Sprintf("10.%d.%d.%d:4000", i>>16&255, i>>8&255, i&255)
		req.Header.Set("Authorization", "Bearer sk-not-a-real-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusTooManyRequests {
			t.Fatalf("bad Bearer %d: %d", i, rec.Code)
		}
		// The real release route, from one valid API key (S5).
		rreq := httptest.NewRequest(http.MethodPost, "/v1/sessions/release", strings.NewReader(`{"model":"test-model"}`))
		rreq.RemoteAddr = "192.0.2.50:4000"
		rreq.Header.Set("Authorization", "Bearer "+floodKey)
		rrec := httptest.NewRecorder()
		h.ServeHTTP(rrec, rreq)
		if rrec.Code != http.StatusOK {
			t.Fatalf("release %d: %d %s", i, rrec.Code, rrec.Body.String())
		}
		if i%997 == 0 {
			if n := srv.opsAgg.Size(); n > maxSeen {
				maxSeen = n
			}
		}
	}
	clk.Advance(2 * time.Minute)
	srv.FlushAudit()
	if maxSeen > bound {
		t.Fatalf("aggregation map reached %d, bound %d", maxSeen, bound)
	}
	if n := srv.lockout.TrackedBearerIPs(); n > 10000 {
		t.Fatalf("bearer lockout map %d entries", n)
	}
	sec := readStream(t, cfg.DataDir, AuditFileSecurity)
	if !hasKind(sec, "login", "ok") || !hasKind(sec, "key_reveal", "ok") {
		t.Fatal("login/key_reveal lines lost from the security file")
	}
	var total int64
	for _, s := range []string{"", ".1", ".2", ".3"} {
		fi, err := os.Lstat(filepath.Join(cfg.DataDir, AuditFileOps+s))
		if err != nil {
			continue
		}
		if s == ".3" {
			t.Fatal("more than 3 ops files")
		}
		if fi.Size() > 5<<20 {
			t.Fatalf("%s is %d bytes", AuditFileOps+s, fi.Size())
		}
		total += fi.Size()
	}
	if total > 15<<20 {
		t.Fatalf("ops stream %d bytes, over 15 MiB", total)
	}
	evs := auditEvents(t, ac, ts)
	if !hasKind(evs, "login", "ok") || !hasKind(evs, "key_reveal", "ok") {
		t.Fatal("login/key_reveal lost from GET /admin/audit")
	}
	if !hasKind(evs, "admin_bearer_reject", "") {
		t.Fatal("rejects not visible at all")
	}
}

// S2-4: retention runs at boot over rotated files and the live file is
// never rewritten.
func TestS2RetentionAtBoot(t *testing.T) {
	dir := t.TempDir()
	old := func(name string, days int) {
		b, _ := json.Marshal(AuditEvent{At: time.Now().Add(-time.Duration(days) * 24 * time.Hour).UTC(), Kind: "login", Result: "ok", Message: "login ok", Stream: AuditStreamHighValue})
		if err := os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old(AuditFileSecurity+".2", 100) // > 90: pruned
	old(AuditFileSecurity+".1", 60)  // kept
	old(AuditFileOps+".1", 31)       // > 30: pruned
	old(AuditFileOps, 400)           // live: kept
	cfg := &config.Config{DataDir: dir}
	ring := NewAuditLog(500)
	openAuditSink(ring, dir, time.Duration(auditRetentionDays(cfg))*24*time.Hour)
	for name, want := range map[string]bool{
		AuditFileSecurity + ".2": false, AuditFileSecurity + ".1": true,
		AuditFileOps + ".1": false, AuditFileOps: true,
	} {
		if _, err := os.Lstat(filepath.Join(dir, name)); (err == nil) != want {
			t.Errorf("%s exists=%v want %v", name, err == nil, want)
		}
	}
	if auditRetentionDays(&config.Config{AuditRetentionDays: 7}) != 7 {
		t.Fatal("AUDIT_RETENTION_DAYS not used")
	}
}

// S2-4: a symlinked audit file is refused (ring only, no crash).
func TestS2SymlinkRefusedRingOnly(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	_ = os.WriteFile(target, nil, 0o600)
	if err := os.Symlink(target, filepath.Join(dir, AuditFileSecurity)); err != nil {
		t.Fatal(err)
	}
	ring := NewAuditLog(500)
	a := openAuditSink(ring, dir, 90*24*time.Hour)
	a.WriteAudit(AuditStreamHighValue, AuditEvent{Kind: "login", Result: "ok", Message: "login ok"})
	if b, _ := os.ReadFile(target); len(b) != 0 {
		t.Fatal("wrote through the symlink")
	}
	if !hasKind(ring.Recent(10), "login", "ok") {
		t.Fatal("ring lost the event")
	}
	if a.diskStatus()["persistent"] != false {
		t.Fatal("disk status claims a persistent audit")
	}
}

// S2-4: disk status includes the audit files.
func TestS2DiskStatusIncludesAudit(t *testing.T) {
	ts, _, cfg, _ := s1Server(t, nil)
	ac := adminClient(t, ts, cfg)
	resp, err := ac.Get(ts.URL + "/admin/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st struct {
		Disk struct {
			Audit struct {
				SecurityBytes int64 `json:"securityBytes"`
				Persistent    bool  `json:"persistent"`
			} `json:"audit"`
		} `json:"disk"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Disk.Audit.SecurityBytes <= 0 || !st.Disk.Audit.Persistent {
		t.Fatalf("disk audit: %+v", st.Disk.Audit)
	}
}

// S2-5: no token, no part of it, no hash of it; path only, sanitised; no
// User-Agent.
func TestS2NoTokenMaterialInAudit(t *testing.T) {
	ts, srv, cfg, _ := s1Server(t, func(c *config.Config) { c.MasterKeyAdmin = true })
	const body = "zqxjVBNMwkplrtyhfd82"
	bad := "sk-" + body
	master := keymaker.MasterKey(cfg.APIKeySeed)
	send := func(path, tok string) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("User-Agent", "UAMARKER-77")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	send("/admin/pool/close/ab%0Acd%1Bef?key="+body, bad)
	send("/admin/pool/close-all", master)
	srv.FlushAudit()
	ac := adminClient(t, ts, cfg)
	ring, _ := json.Marshal(auditEvents(t, ac, ts))
	all := rawFiles(t, cfg.DataDir) + string(ring)
	for _, tok := range []string{bad, master} {
		raw := strings.TrimPrefix(tok, "sk-")
		sum := sha256.Sum256([]byte(tok))
		hx := hex.EncodeToString(sum[:])
		for _, needle := range []string{tok, raw, raw[:4], raw[len(raw)-4:], hx, hx[:16]} {
			if strings.Contains(all, needle) {
				t.Fatalf("audit contains token material %q", needle)
			}
		}
	}
	if strings.Contains(all, "UAMARKER") || strings.Contains(all, "key=") || strings.ContainsAny(all, "\x01\x1b") || strings.Contains(all, "\\n") {
		t.Fatal("audit has a User-Agent, query or control characters")
	}
	if !strings.Contains(all, "path=/admin/pool/close/ab_cd_ef") {
		t.Fatalf("sanitised path missing: %s", all)
	}
	if !strings.Contains(all, `"kind":"admin_key_use"`) || !strings.Contains(all, `"kind":"pool_close_all"`) {
		t.Fatal("master Bearer use not audited")
	}
}

// S2-6: the Bearer branch's lockout is per-IP only: with the global trip
// active a valid master Bearer from another address still reaches the
// close routes; 25 bad Bearers lock only that address.
func TestS2BearerLockoutPerIPOnly(t *testing.T) {
	ts, srv, cfg, _ := s1Server(t, func(c *config.Config) {
		c.MasterKeyAdmin = true
		c.TrustedProxyCIDRs = []string{"127.0.0.0/8"}
	})
	master := keymaker.MasterKey(cfg.APIKeySeed)
	do := func(method, path, tok, ip string) int {
		req, _ := http.NewRequest(method, ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i < 25; i++ {
		srv.lockout.Failure(fmt.Sprintf("198.51.100.%d", i))
	}
	if !srv.lockout.GlobalLocked() {
		t.Fatal("global trip not active")
	}
	if got := do(http.MethodPost, "/admin/pool/close-all", master, "203.0.113.9"); got != http.StatusOK {
		t.Fatalf("master Bearer under global trip: %d", got)
	}
	if got := do(http.MethodPost, "/admin/pool/close/s-unknown", master, "203.0.113.9"); got == http.StatusUnauthorized || got == http.StatusTooManyRequests {
		t.Fatalf("close/{id} under global trip: %d", got)
	}
	srv.lockout.Reset()

	var codes []int
	for i := 0; i < 25; i++ {
		codes = append(codes, do(http.MethodGet, "/admin/status", "sk-wrong", "192.0.2.66"))
	}
	if codes[0] != http.StatusUnauthorized || codes[24] != http.StatusTooManyRequests {
		t.Fatalf("bad Bearer codes: %v", codes)
	}
	if srv.lockout.GlobalLocked() {
		t.Fatal("Bearer failures fed the global trip")
	}
	if ok, _ := srv.lockout.Allow("192.0.2.66"); !ok {
		t.Fatal("Bearer failures locked the password login")
	}
	if got := do(http.MethodGet, "/admin/status", master, "192.0.2.67"); got != http.StatusOK {
		t.Fatalf("other address locked: %d", got)
	}
	if got := do(http.MethodGet, "/admin/status", master, "192.0.2.66"); got != http.StatusTooManyRequests {
		t.Fatalf("locked address answered %d before the compare", got)
	}
}

// S2-7: a write error is logged and not fatal: the server keeps answering
// with the ring.
func TestS2DiskFullNotFatal(t *testing.T) {
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skip("/dev/full not available")
	}
	full.Close()
	ts, srv, cfg, _ := s1Server(t, nil)
	srv.auditFile.security.SwapFileForTest("/dev/full")
	srv.auditFile.ops.SwapFileForTest("/dev/full")
	for i := 0; i < 3; i++ {
		login(t, ts, "wrong-password-x", "")
	}
	ac := adminClient(t, ts, cfg)
	if httpCode(t, ac, ts, "/admin/keys?reveal=1") != http.StatusOK {
		t.Fatal("reveal on a full disk")
	}
	if evs := auditEvents(t, ac, ts); !hasKind(evs, "login", "ok") || !hasKind(evs, "key_reveal", "ok") {
		t.Fatal("ring lost events on a full disk")
	}
}
