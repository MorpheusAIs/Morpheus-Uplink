package store

import (
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
	full, rec, err := keymaker.NewKey("gary")
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
	if keys[0].Name != "gary" || keys[0].ID != rec.ID || keys[0].Hash != rec.Hash {
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
