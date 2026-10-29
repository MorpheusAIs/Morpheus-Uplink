package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/router"
)

// The pool's sleep seam: a real, ctx-bounded sleep by default; SetSleep
// routes every wait to the hook; SetSleep(nil) puts the real sleep back.
func TestSleepSeam(t *testing.T) {
	p := New(router.New("http://127.0.0.1:9", "u", "p"), 600)
	ctx := context.Background()
	start := time.Now()
	if err := p.sleep(ctx, 5*time.Millisecond); err != nil || time.Since(start) < 5*time.Millisecond {
		t.Fatalf("default sleep: err %v after %v, want a real 5ms sleep", err, time.Since(start))
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	start = time.Now()
	if err := p.sleep(cctx, time.Hour); !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
		t.Fatalf("cancelled sleep: %v after %v, want context.Canceled at once", err, time.Since(start))
	}
	var got []time.Duration
	p.SetSleep(func(_ context.Context, d time.Duration) error { got = append(got, d); return nil })
	if err := p.sleep(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	p.SetSleep(nil)
	start = time.Now()
	_ = p.sleep(ctx, 2*time.Millisecond)
	if time.Since(start) < 2*time.Millisecond || len(got) != 1 || got[0] != time.Hour {
		t.Fatalf("hook saw %v; SetSleep(nil) must restore the real sleep", got)
	}
}
