package config

import "testing"

// auditRetentionDays: default 90; a saved value in range applies at boot,
// an invalid one is skipped (default kept).
func TestAuditRetentionDays(t *testing.T) {
	for in, want := range map[string]int{"30": 30, "1": 1, "3650": 3650, "0": 90, "-5": 90, "3651": 90, "ninety": 90, " 45 ": 45} {
		clearSettingEnv(t)
		c := quietFromEnv(t)
		if c.AuditRetentionDays != 90 {
			t.Fatalf("default %d", c.AuditRetentionDays)
		}
		ApplyPersisted(c, map[string]string{"auditRetentionDays": in})
		if c.AuditRetentionDays != want {
			t.Errorf("auditRetentionDays=%q → %d, want %d", in, c.AuditRetentionDays, want)
		}
	}
}
