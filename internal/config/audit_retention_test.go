package config

import "testing"

// AUDIT_RETENTION_DAYS: default 90; a bad or out-of-range value falls back
// to 90 (with a warning).
func TestAuditRetentionDays(t *testing.T) {
	for in, want := range map[string]int{"": 90, "30": 30, "1": 1, "3650": 3650, "0": 90, "-5": 90, "3651": 90, "ninety": 90, " 45 ": 45} {
		setRequired(t)
		t.Setenv("ROUTER_AUTH", "user:router-pass-x")
		t.Setenv("AUDIT_RETENTION_DAYS", in)
		c, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if c.AuditRetentionDays != want {
			t.Errorf("AUDIT_RETENTION_DAYS=%q → %d, want %d", in, c.AuditRetentionDays, want)
		}
	}
}
