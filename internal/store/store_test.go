package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/keymaker"
)

func TestPruneUsageOlderThanKeepsKeysAndOpenSessions(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	rec := keymaker.Record{
		ID:        "abcd1234",
		Name:      "test",
		Prefix:    "sk-abcd1234.",
		Hash:      "deadbeef",
		Secret:    "sk-abcd1234.secret",
		CreatedAt: time.Now().UTC(),
	}
	if err := s.AddKey(rec); err != nil {
		t.Fatal(err)
	}
	s.TrackSessionOpen("0xsession", "abcd1234", "test-model")

	oldDay := time.Now().UTC().AddDate(0, 0, -45).Format("2006-01-02")
	recentDay := time.Now().UTC().AddDate(0, 0, -5).Format("2006-01-02")
	s.mu.Lock()
	s.st.Usage[oldDay+"|abcd1234|old-model"] = &UsageRow{
		Day: oldDay, KeyID: "abcd1234", Model: "old-model", Requests: 3,
	}
	s.st.Usage[recentDay+"|abcd1234|new-model"] = &UsageRow{
		Day: recentDay, KeyID: "abcd1234", Model: "new-model", Requests: 1,
	}
	if err := s.save(); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.mu.Unlock()

	removed, err := s.PruneUsageOlderThan(30)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed: got %d want 1", removed)
	}

	keys := s.ListKeys()
	if len(keys) != 1 || keys[0].ID != "abcd1234" {
		t.Fatalf("keys mutated: %+v", keys)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.st.OpenSessions["0xsession"]; !ok {
		t.Fatal("OpenSessions entry was removed")
	}
	if _, ok := s.st.Usage[oldDay+"|abcd1234|old-model"]; ok {
		t.Fatal("old usage row still present")
	}
	if _, ok := s.st.Usage[recentDay+"|abcd1234|new-model"]; !ok {
		t.Fatal("recent usage row was pruned")
	}
	if filepath.Base(s.Path()) != "uplink-state.json" {
		t.Fatalf("unexpected path %s", s.Path())
	}
}

func TestPruneUsageOlderThanNoOp(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := s.PruneUsageOlderThan(30)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed: got %d want 0", removed)
	}
}

func TestRevokeKeyTombstone(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	full, rec, err := keymaker.NewKey("operator")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddKey(rec); err != nil {
		t.Fatal(err)
	}
	if id := s.LookupKey(full); id != rec.ID {
		t.Fatalf("LookupKey before revoke: got %q want %q", id, rec.ID)
	}

	ok, err := s.RevokeKey(rec.ID)
	if err != nil || !ok {
		t.Fatalf("RevokeKey: ok=%v err=%v", ok, err)
	}
	keys := s.ListKeys()
	if len(keys) != 1 {
		t.Fatalf("expected tombstone retained, got %d keys", len(keys))
	}
	if keys[0].RevokedAt == nil {
		t.Fatal("RevokedAt unset")
	}
	if keys[0].Secret != "" {
		t.Fatalf("secret should be cleared, got %q", keys[0].Secret)
	}
	if keys[0].Name != "operator" || keys[0].ID != rec.ID || keys[0].Hash != rec.Hash {
		t.Fatalf("tombstone fields mutated: %+v", keys[0])
	}
	if id := s.LookupKey(full); id != "" {
		t.Fatalf("LookupKey after revoke: got %q want empty", id)
	}

	// Idempotent second revoke.
	ok, err = s.RevokeKey(rec.ID)
	if err != nil || !ok {
		t.Fatalf("idempotent RevokeKey: ok=%v err=%v", ok, err)
	}

	// Missing id → false.
	ok, err = s.RevokeKey("no-such-id")
	if err != nil || ok {
		t.Fatalf("missing: ok=%v err=%v", ok, err)
	}
}

func TestRevokeKeyRefusesMasterPrompt(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{keymaker.MasterKeyID, keymaker.PromptKeyID} {
		ok, err := s.RevokeKey(id)
		if err == nil || ok {
			t.Fatalf("revoke %s: ok=%v err=%v", id, ok, err)
		}
	}
}

func TestAddKeyRejectsTombstoneCollision(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, rec, err := keymaker.NewKey("orig")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddKey(rec); err != nil {
		t.Fatal(err)
	}
	ok, err := s.RevokeKey(rec.ID)
	if err != nil || !ok {
		t.Fatalf("revoke: ok=%v err=%v", ok, err)
	}

	// Same id + new hash
	dupID := rec
	dupID.Hash = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	dupID.Secret = "sk-revived.should-fail"
	dupID.RevokedAt = nil
	if err := s.AddKey(dupID); err == nil {
		t.Fatal("expected id collision with tombstone")
	}

	// Same hash + new id
	dupHash := rec
	dupHash.ID = "deadbeef"
	dupHash.Secret = "sk-deadbeef.should-fail"
	dupHash.RevokedAt = nil
	if err := s.AddKey(dupHash); err == nil {
		t.Fatal("expected hash collision with tombstone")
	}
}

// A state file that cannot be parsed stops Open (the service refuses to
// boot) instead of starting with empty auth state, which would mean 2FA off.
func TestOpenRefusesMalformedStateFile(t *testing.T) {
	for name, body := range map[string]string{
		"not_json":        `{"auth":{"enrolled":true,`,
		"wrong_type":      `{"auth":{"twoFactorOn":"yes"}}`,
		"auth_not_object": `{"auth":[]}`,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "uplink-state.json"), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir); err == nil {
			t.Fatalf("%s: Open accepted a malformed state file", name)
		}
	}
}

func TestAuthStateOn(t *testing.T) {
	for _, tc := range []struct {
		a    AuthState
		want bool
	}{
		{AuthState{}, false},
		{AuthState{TwoFactorOn: true}, true},
		{AuthState{TwoFactorOn: true, Enrolled: true}, true},
		{AuthState{Enrolled: true}, true}, // secret without the flag: fail closed
	} {
		if got := tc.a.On(); got != tc.want {
			t.Fatalf("%+v: On()=%v want %v", tc.a, got, tc.want)
		}
	}
}

// Security gate S-3: save writes a fresh temp file (a planted *.tmp, symlinked or
// world-writable, has no effect), leaves the state file at 0640, and a
// symlinked state file is refused at open and at save.
func TestSaveIgnoresPlantedTmp(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "uplink-state.json")
	if err := os.Symlink(victim, statePath+".tmp"); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(dir, "uplink-state.json.x.tmp")
	if err := os.WriteFile(planted, []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(planted, 0o666); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSettings(func(st *Settings) error { st.LogLevel = "warn"; return nil }); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("symlinked tmp followed: victim now %q", b)
	}
	if b, _ := os.ReadFile(planted); string(b) != "planted" {
		t.Fatalf("planted tmp reused: %q", b)
	}
	fi, err := os.Lstat(statePath)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm() != 0o640 {
		t.Fatalf("state file %v %v", fi.Mode(), err)
	}
	if s2, err := Open(dir); err != nil || s2.GetSettings().LogLevel != "warn" {
		t.Fatalf("reopen: %v", err)
	}
	// No temp file of ours is left behind.
	m, _ := filepath.Glob(filepath.Join(dir, "uplink-state.json.*.tmp"))
	if len(m) != 1 || m[0] != planted {
		t.Fatalf("temp files left: %v", m)
	}
}

func TestSymlinkedStateFileRefused(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"keys":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "uplink-state.json")
	if err := os.Symlink(target, statePath); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("open accepted a symlinked state file")
	}
	// A symlink swapped in after open is refused at save too.
	dir2 := t.TempDir()
	s, err := Open(dir2)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir2, "uplink-state.json")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSettings(func(st *Settings) error { st.LogLevel = "warn"; return nil }); err == nil {
		t.Fatal("save accepted a symlinked state file")
	}
	if b, _ := os.ReadFile(target); string(b) != `{"keys":[]}` {
		t.Fatalf("target written: %q", b)
	}
}
