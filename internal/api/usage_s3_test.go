package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/absgrafx/morpheus-uplink/internal/keymaker"
)

func chatAs(t *testing.T, ts *httptest.Server, bearer, model string) {
	t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat %s: %d", model, resp.StatusCode)
	}
}

func usageFrom(t *testing.T, c *http.Client, url string, bearer string) []UsageView {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Usage []UsageView `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Usage
}

// S3-1, S3-2: name and id calls are recorded by resolved id and roll up into
// one row labelled "name (0x + 6 hex)" in /admin/usage and /v1/usage.
func TestS3NameAndIDCallsShareOneRow(t *testing.T) {
	ts, srv, cfg, _ := s1Server(t, nil)
	master := keymaker.MasterKey(cfg.APIKeySeed)
	chatAs(t, ts, master, "test-model")
	chatAs(t, ts, master, testModelID)
	chatAs(t, ts, master, strings.ToUpper(testModelID[:2])[:1]+"x"+strings.ToUpper(testModelID[2:]))
	for _, r := range srv.store.Usage() {
		if r.Model != testModelID && !strings.EqualFold(r.Model, testModelID) {
			t.Fatalf("stored by %q, want the resolved id", r.Model)
		}
	}
	want := "test-model (" + testModelID[:8] + ")"
	if want != "test-model (0xabc123)" {
		t.Fatalf("label form %q", want)
	}
	ac := adminClient(t, ts, cfg)
	for name, rows := range map[string][]UsageView{
		"/admin/usage": usageFrom(t, ac, ts.URL+"/admin/usage", ""),
		"/v1/usage":    usageFrom(t, http.DefaultClient, ts.URL+"/v1/usage", master),
	} {
		if len(rows) != 1 || rows[0].Model != want || rows[0].ModelID != testModelID || rows[0].Requests != 3 {
			t.Fatalf("%s rows: %+v", name, rows)
		}
	}
}

// S3-3: an unknown or delisted id falls back to the short id.
func TestS3UnknownIDShowsShortID(t *testing.T) {
	_, srv, _, _ := s1Server(t, nil)
	gone := "0xdeadbeef" + strings.Repeat("0", 56)
	srv.store.RecordUsage("k1", gone, 1, 1)
	rows := srv.usageViews(srv.store.Usage())
	if len(rows) != 1 || rows[0].Model != "0xdeadbe" || rows[0].ModelID != gone {
		t.Fatalf("rows: %+v", rows)
	}
}

// S3-4: rows stored by name before this change migrate on read: a name the
// catalog still resolves joins the id row (requests, tokens and session
// time, which feeds the "Session time by model" chart); an unresolvable
// name stays as it was.
func TestS3LegacyNameRowsMigrateOnRead(t *testing.T) {
	_, srv, _, _ := s1Server(t, nil)
	srv.store.RecordUsage("k1", "test-model", 10, 20) // legacy (name)
	srv.store.RecordUsage("k1", testModelID, 1, 2)    // new (id)
	srv.store.TrackSessionOpen("0xsess01", "k1", "test-model")
	srv.store.RecordSessionClose("0xsess01", 30, "")
	srv.store.TrackSessionOpen("0xsess02", "k1", testModelID)
	srv.store.RecordSessionClose("0xsess02", 12, "")
	srv.store.RecordUsage("k1", "retired-model", 5, 5)
	rows := srv.usageViews(srv.store.Usage())
	var hit, old *UsageView
	for i := range rows {
		switch rows[i].Model {
		case "test-model (0xabc123)":
			hit = &rows[i]
		case "retired-model":
			old = &rows[i]
		}
	}
	if len(rows) != 2 || hit == nil || old == nil {
		t.Fatalf("rows: %+v", rows)
	}
	if hit.Requests != 2 || hit.PromptTokens != 11 || hit.CompletionTokens != 22 || hit.SessionSeconds != 42 {
		t.Fatalf("merged row: %+v", hit)
	}
	if old.ModelID != "" || old.Requests != 1 {
		t.Fatalf("unresolvable name row: %+v", old)
	}
}
