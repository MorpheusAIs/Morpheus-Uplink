package pool

import (
	"testing"
	"time"
)

// The sleep seam defaults to a real sleep, routes through SetSleep's hook,
// and restore puts the previous hook back.
func TestSleepSeam(t *testing.T) {
	start := time.Now()
	sleep(5 * time.Millisecond)
	if el := time.Since(start); el < 5*time.Millisecond {
		t.Fatalf("default sleep returned after %v, want a real 5ms sleep", el)
	}
	var got []time.Duration
	restore := SetSleep(func(d time.Duration) { got = append(got, d) })
	sleep(time.Hour)
	inner := SetSleep(nil) // nil: real sleep
	start = time.Now()
	sleep(2 * time.Millisecond)
	if time.Since(start) < 2*time.Millisecond {
		t.Fatal("SetSleep(nil) did not restore the real sleep")
	}
	inner()
	sleep(time.Minute)
	restore()
	if len(got) != 2 || got[0] != time.Hour || got[1] != time.Minute {
		t.Fatalf("hook saw %v, want [1h 1m]", got)
	}
	start = time.Now()
	sleep(2 * time.Millisecond)
	if len(got) != 2 || time.Since(start) < 2*time.Millisecond {
		t.Fatal("restore did not put the real sleep back")
	}
}
