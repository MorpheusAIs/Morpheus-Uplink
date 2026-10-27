package auditlog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type line struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	N    int       `json:"n"`
}

func TestOpenCreates0600AppendOnly(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir, "audit.jsonl", Options{})
	defer s.Close()
	if s.Disabled() {
		t.Fatal("disabled")
	}
	s.Append(line{Kind: "a"})
	s.Close()
	s2 := Open(dir, "audit.jsonl", Options{})
	s2.Append(line{Kind: "b"})
	s2.Close()
	fi, err := os.Stat(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if strings.Count(string(b), "\n") != 2 || !strings.Contains(string(b), `"kind":"a"`) {
		t.Fatalf("not appended: %q", b)
	}
}

func TestOpenTightensExistingMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "audit.jsonl")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := Open(dir, "audit.jsonl", Options{})
	defer s.Close()
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestOpenRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "audit.jsonl")); err != nil {
		t.Fatal(err)
	}
	s := Open(dir, "audit.jsonl", Options{})
	if !s.Disabled() {
		t.Fatal("symlink accepted")
	}
	s.Append(line{Kind: "x"}) // dropped, no panic
	if b, _ := os.ReadFile(target); string(b) != "keep\n" {
		t.Fatalf("symlink target written: %q", b)
	}
}

func TestOpenRefusesNonRegular(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "audit.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if s := Open(dir, "audit.jsonl", Options{}); !s.Disabled() {
		t.Fatal("directory accepted")
	}
}

type fakeInfo struct {
	fs.FileInfo
	uid uint32
}

func (f fakeInfo) Mode() fs.FileMode { return 0o600 }
func (f fakeInfo) Sys() any          { return &syscall.Stat_t{Uid: f.uid} }

func TestOwnerCheckRefusesOtherUID(t *testing.T) {
	if err := checkOwned(fakeInfo{uid: uint32(os.Getuid())}); err != nil {
		t.Fatalf("own uid refused: %v", err)
	}
	if err := checkOwned(fakeInfo{uid: uint32(os.Getuid()) + 1}); err == nil {
		t.Fatal("file owned by another uid accepted")
	}
	// As root the real path is testable too.
	if os.Getuid() == 0 {
		dir := t.TempDir()
		p := filepath.Join(dir, "audit.jsonl")
		_ = os.WriteFile(p, nil, 0o600)
		if err := os.Chown(p, 65534, 65534); err != nil {
			t.Skip(err)
		}
		if s := Open(dir, "audit.jsonl", Options{}); !s.Disabled() {
			t.Fatal("other-uid file accepted")
		}
	}
}

func TestRotateKeepsAtMostThreeFiles(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir, "audit.jsonl", Options{MaxBytes: 1000})
	defer s.Close()
	for i := 0; i < 500; i++ {
		s.Append(line{At: time.Unix(int64(i), 0).UTC(), Kind: "k", N: i})
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 3 {
		t.Fatalf("files: %d, want 3", len(ents))
	}
	for _, e := range ents {
		fi, _ := e.Info()
		if fi.Size() > 1000 {
			t.Fatalf("%s is %d bytes, over the cap", e.Name(), fi.Size())
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", e.Name(), fi.Mode().Perm())
		}
	}
	// Tail is oldest-first across .2, .1 and the live file, and ends with
	// the newest line.
	got := s.Tail(1000)
	var first, last line
	_ = json.Unmarshal(got[0], &first)
	_ = json.Unmarshal(got[len(got)-1], &last)
	if last.N != 499 || first.N >= last.N {
		t.Fatalf("tail order: first %d last %d", first.N, last.N)
	}
	for i := 1; i < len(got); i++ {
		var a, b line
		_ = json.Unmarshal(got[i-1], &a)
		_ = json.Unmarshal(got[i], &b)
		if b.N != a.N+1 {
			t.Fatalf("tail gap at %d: %d then %d", i, a.N, b.N)
		}
	}
	if n := len(s.Tail(5)); n != 5 {
		t.Fatalf("Tail(5) = %d lines", n)
	}
	if s.Bytes() <= 0 || s.Bytes() > 3000 {
		t.Fatalf("Bytes = %d", s.Bytes())
	}
}

func TestPruneDeletesOnlyOldRotatedFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	write := func(name string, at time.Time) {
		b, _ := json.Marshal(line{At: at, Kind: "k"})
		if err := os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("audit.jsonl.2", now.Add(-40*24*time.Hour))
	write("audit.jsonl.1", now.Add(-10*24*time.Hour))
	write("audit.jsonl", now.Add(-400*24*time.Hour)) // live: never pruned
	s := Open(dir, "audit.jsonl", Options{Retention: 30 * 24 * time.Hour})
	defer s.Close()
	s.now = func() time.Time { return now }
	if n := s.Prune(); n != 1 {
		t.Fatalf("pruned %d, want 1", n)
	}
	for name, want := range map[string]bool{"audit.jsonl.2": false, "audit.jsonl.1": true, "audit.jsonl": true} {
		_, err := os.Stat(filepath.Join(dir, name))
		if (err == nil) != want {
			t.Fatalf("%s exists=%v, want %v", name, err == nil, want)
		}
	}
	live, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if !bytes.Contains(live, []byte(`"kind":"k"`)) {
		t.Fatal("live file rewritten")
	}
}

// countHandler counts warnings.
type countHandler struct{ warns atomic.Int32 }

func (h *countHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		h.warns.Add(1)
	}
	return nil
}
func (h *countHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countHandler) WithGroup(string) slog.Handler      { return h }

// A full disk: writes fail, one warning is logged, nothing panics.
func TestWriteErrorLoggedOnceNotFatal(t *testing.T) {
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skip("/dev/full not available")
	}
	h := &countHandler{}
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(old)
	s := Open(t.TempDir(), "audit.jsonl", Options{})
	s.mu.Lock()
	s.f.Close()
	s.f = full
	s.mu.Unlock()
	for i := 0; i < 100; i++ {
		s.Append(line{Kind: fmt.Sprint(i)})
	}
	if n := h.warns.Load(); n != 1 {
		t.Fatalf("warnings: %d, want 1", n)
	}
	_ = s.Close()
}
