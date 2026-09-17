package api

import (
	"testing"
)

func TestDiskStatRoot(t *testing.T) {
	st, err := diskStat("/")
	if err != nil {
		t.Fatalf("diskStat(/): %v", err)
	}
	if st.Path != "/" {
		t.Fatalf("path: got %q", st.Path)
	}
	if st.Total == 0 {
		t.Fatal("expected non-zero total for /")
	}
	if st.Free > st.Total {
		t.Fatalf("free %d > total %d", st.Free, st.Total)
	}
	if st.Used+st.Free > st.Total+st.Total/100 { // allow minor rounding via Bavail vs Blocks
		// Used is total-free(Bavail); Bavail can be less than Bfree, so used+free may be < total.
	}
	wantUsed := st.Total - st.Free
	if st.Used != wantUsed {
		t.Fatalf("used: got %d want %d", st.Used, wantUsed)
	}
	if st.FreePct < 0 || st.FreePct > 100 {
		t.Fatalf("freePct out of range: %v", st.FreePct)
	}
	expectWarn := st.Free < diskWarnFreeBytes || st.FreePct < diskWarnFreePct
	if st.Warn != expectWarn {
		t.Fatalf("warn: got %v want %v (free=%d freePct=%v)", st.Warn, expectWarn, st.Free, st.FreePct)
	}
}

func TestDiskStatTempDir(t *testing.T) {
	dir := t.TempDir()
	st, err := diskStat(dir)
	if err != nil {
		t.Fatalf("diskStat(temp): %v", err)
	}
	if st.Total == 0 {
		t.Fatal("expected non-zero total")
	}
}

func TestDiskStatMissing(t *testing.T) {
	_, err := diskStat("/no/such/path/uplink-disk-test-" + t.Name())
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestCollectDiskStatusWarnAggregate(t *testing.T) {
	out := collectDiskStatus("/", t.TempDir())
	paths, _ := out["paths"].([]diskPathStat)
	if len(paths) != 2 {
		t.Fatalf("paths: got %d want 2", len(paths))
	}
	_, warnOK := out["warn"].(bool)
	if !warnOK {
		t.Fatal("missing warn bool")
	}
}
