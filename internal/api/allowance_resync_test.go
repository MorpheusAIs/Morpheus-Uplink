package api

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeManagerStatus(t *testing.T, path, mode, wei string, hb time.Time) {
	t.Helper()
	b, _ := json.Marshal(allowanceStatusFile{
		Mode: mode, Source: mode, Wei: wei, BalanceWei: wei,
		LastResyncAt: hb.UTC().Format(time.RFC3339), LastResult: "ok", LastReason: "poll",
		HeartbeatAt: hb.UTC().Format(time.RFC3339),
	})
	if err := os.WriteFile(path, b, 0o444); err != nil && !os.IsPermission(err) {
		t.Fatal(err)
	}
}

func postResync(t *testing.T, c *http.Client, ts string, cfgURL string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts+"/admin/allowance/resync", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", cfgURL)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

// Status shows auto-resync only in balance mode with a fresh heartbeat, and
// reports the manager's current allowance (design: GUI button gating, T9).
func TestAllowanceResyncStatusGating(t *testing.T) {
	rt := rejectingRouter(t)
	defer rt.Close()
	ts, cfg := newServerWithRouter(t, rt, "")
	dir := t.TempDir()
	cfg.AllowanceStatusFile = filepath.Join(dir, "allowance-status.json")
	cfg.AllowanceCtlDir = t.TempDir()
	cfg.AllowancePollSec = 60
	cfg.MORAllowanceWei = "100"
	cfg.MORAllowanceSource = "balance"
	c := adminClient(t, ts, cfg)

	get := func() (map[string]any, map[string]any) {
		st := getStatus(t, c, ts)
		ar, _ := st["allowanceResync"].(map[string]any)
		ma, _ := st["morAllowance"].(map[string]any)
		return ar, ma
	}
	// Manager absent (T9: manager down => button hidden, Uplink still serves).
	if ar, _ := get(); ar["available"] != false {
		t.Fatalf("absent manager: %v", ar)
	}
	writeManagerStatus(t, cfg.AllowanceStatusFile, "balance", "4200", time.Now())
	ar, ma := get()
	if ar["available"] != true || ma["wei"] != "4200" || ma["bootWei"] != "100" {
		t.Fatalf("fresh balance: %v / %v", ar, ma)
	}
	os.Remove(cfg.AllowanceStatusFile)
	writeManagerStatus(t, cfg.AllowanceStatusFile, "balance", "4200", time.Now().Add(-10*time.Minute))
	if ar, ma := get(); ar["available"] != false || ar["reason"] != "manager not running" || ma["wei"] != "100" {
		t.Fatalf("stale heartbeat: %v / %v", ar, ma)
	}
	os.Remove(cfg.AllowanceStatusFile)
	writeManagerStatus(t, cfg.AllowanceStatusFile, "cap", "", time.Now())
	if ar, _ := get(); ar["available"] != false {
		t.Fatalf("cap mode: %v", ar)
	}
}

// POST /admin/allowance/resync: presence-only flag via O_EXCL|O_NOFOLLOW,
// rate limited, audited; unavailable => 409; planted symlink not followed.
func TestAllowanceResyncRequest(t *testing.T) {
	rt := rejectingRouter(t)
	defer rt.Close()
	ts, cfg := newServerWithRouter(t, rt, "")
	cfg.AllowanceStatusFile = filepath.Join(t.TempDir(), "allowance-status.json")
	ctl := t.TempDir()
	cfg.AllowanceCtlDir = ctl
	cfg.AllowancePollSec = 60
	c := adminClient(t, ts, cfg)

	if code, _ := postResync(t, c, ts.URL, cfg.WebPublicURL); code != http.StatusConflict {
		t.Fatalf("unavailable: code=%d", code)
	}
	writeManagerStatus(t, cfg.AllowanceStatusFile, "balance", "1", time.Now())
	code, out := postResync(t, c, ts.URL, cfg.WebPublicURL)
	if code != http.StatusAccepted || out["requested"] != true {
		t.Fatalf("request: %d %v", code, out)
	}
	fi, err := os.Lstat(filepath.Join(ctl, resyncFlagName))
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != 0 {
		t.Fatalf("flag: %v %v", fi, err)
	}
	if code, _ := postResync(t, c, ts.URL, cfg.WebPublicURL); code != http.StatusTooManyRequests {
		t.Fatalf("rate limit: code=%d", code)
	}
	evs := auditEvents(t, c, ts)
	if !hasAudit(evs, "allowance_resync_request", "ok", "") || !hasAudit(evs, "allowance_resync_request", "rate_limited", "") {
		t.Fatalf("audit: %+v", evs)
	}
	for _, e := range evs {
		if e.Kind == "allowance_resync_request" && e.Result == "ok" && !strings.Contains(e.Message, "user=admin session=") {
			t.Fatalf("audit lacks user/session: %q", e.Message)
		}
	}

	// A symlink planted as the flag is not followed: EEXIST, target intact.
	os.Remove(filepath.Join(ctl, resyncFlagName))
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(ctl, resyncFlagName)); err != nil {
		t.Fatal(err)
	}
	if err := createResyncFlag(ctl); err == nil {
		t.Fatal("symlink flag was opened")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("symlink target changed: %q", b)
	}
	// A dangling symlink must not create its target either.
	os.Remove(filepath.Join(ctl, resyncFlagName))
	dangling := filepath.Join(t.TempDir(), "created-through-link")
	_ = os.Symlink(dangling, filepath.Join(ctl, resyncFlagName))
	if err := createResyncFlag(ctl); err == nil {
		t.Fatal("dangling symlink flag was opened")
	}
	if _, err := os.Lstat(dangling); !os.IsNotExist(err) {
		t.Fatal("dangling symlink target was created")
	}
}
