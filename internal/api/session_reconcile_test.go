package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/absgrafx/morpheus-uplink/internal/pool"
)

func TestReconcileStatusFileWritten(t *testing.T) {
	dir := t.TempDir()
	st := pool.ReconcileState{State: "partial", Adopted: 2, Closed: 1, Deferred: 3, Skipped: 1}
	if err := writeReconcileStatusFile(dir, st); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, ReconcileStatusFileName)
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("stat %v %v", fi, err)
	}
	var got pool.ReconcileState
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &got); err != nil || got.State != "partial" || got.Deferred != 3 || got.Adopted != 2 {
		t.Fatalf("got %+v %v", got, err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, ".session-reconcile-*"))
	if len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}
