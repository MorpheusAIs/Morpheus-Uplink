package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The GUI warns while MASTER_KEY_ADMIN=true; /admin/status must say so.
func TestStatusReportsMasterKeyAdmin(t *testing.T) {
	ts, cfg, _ := newTestServer(t)
	ac := adminClient(t, ts, cfg)
	for _, on := range []bool{false, true} {
		cfg.MasterKeyAdmin = on
		resp, err := ac.Get(ts.URL + "/admin/status")
		if err != nil {
			t.Fatal(err)
		}
		var st struct {
			TwoFactor map[string]any `json:"twoFactor"`
		}
		err = json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("status: %d %v", resp.StatusCode, err)
		}
		if got, ok := st.TwoFactor["masterKeyAdmin"].(bool); !ok || got != on {
			t.Fatalf("twoFactor.masterKeyAdmin = %#v, want %v", st.TwoFactor["masterKeyAdmin"], on)
		}
	}
}
