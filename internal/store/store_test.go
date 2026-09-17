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
