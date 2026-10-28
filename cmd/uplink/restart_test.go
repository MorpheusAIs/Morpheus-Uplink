package main

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The restart starts the same binary with the same arguments and
// environment, and reports an exec failure instead of returning quietly.
func TestRestartWithSameBinaryArgsEnv(t *testing.T) {
	var gotPath string
	var gotArgv, gotEnv []string
	exec := func(path string, argv, env []string) error {
		gotPath, gotArgv, gotEnv = path, argv, env
		return nil
	}
	argv := []string{"/usr/local/bin/uplink", "-flag"}
	env := []string{"DATA_DIR=/data", "ROUTER_URL=http://proxy-router:8082"}
	if err := restartWith(exec, "/usr/local/bin/uplink", argv, env); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/usr/local/bin/uplink" || !reflect.DeepEqual(gotArgv, argv) || !reflect.DeepEqual(gotEnv, env) {
		t.Fatalf("exec %q %v %v", gotPath, gotArgv, gotEnv)
	}
	if err := restartWith(exec, "/bin/x", nil, nil); err != nil || !reflect.DeepEqual(gotArgv, []string{"/bin/x"}) {
		t.Fatalf("empty argv: %v %v", err, gotArgv)
	}
	failing := func(string, []string, []string) error { return errors.New("boom") }
	if err := restartWith(failing, "/bin/x", argv, env); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("exec error lost: %v", err)
	}
}

// A SIGTERM or SIGINT while a restart drains cancels the restart; the drain
// still runs to the end. Without a signal the restart goes ahead, and a
// normal shutdown stays a shutdown.
func TestDrainThenDecide(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGTERM, syscall.SIGINT} {
		stop := make(chan os.Signal, 1)
		release := make(chan struct{})
		drained := false
		resCh := make(chan bool, 1)
		go func() {
			resCh <- drainThenDecide(true, stop, func() {
				<-release
				drained = true
			})
		}()
		stop <- sig
		// The signal must not end the wait before the drain finishes.
		select {
		case <-resCh:
			t.Fatalf("%v: returned before the drain finished", sig)
		case <-time.After(50 * time.Millisecond):
		}
		close(release)
		if got := <-resCh; got {
			t.Fatalf("%v during drain: restart still requested", sig)
		}
		if !drained {
			t.Fatalf("%v: drain did not finish", sig)
		}
	}

	// Signal already queued when the drain ends: still cancelled.
	stop := make(chan os.Signal, 1)
	stop <- syscall.SIGTERM
	if drainThenDecide(true, stop, func() {}) {
		t.Fatal("queued SIGTERM: restart still requested")
	}

	// No signal: the restart goes ahead after the drain.
	ran := false
	if !drainThenDecide(true, make(chan os.Signal, 1), func() { ran = true }) || !ran {
		t.Fatal("no signal: want drain then restart")
	}

	// Normal shutdown: never turns into a restart.
	stop = make(chan os.Signal, 1)
	stop <- syscall.SIGTERM
	if drainThenDecide(false, stop, func() {}) {
		t.Fatal("shutdown turned into a restart")
	}
}

// stopCounts records how often each step of finishStop ran.
type stopCounts struct{ drain, flush, exec int }

func (c *stopCounts) steps(execErr error, during func()) (func(), func(), func() error) {
	return func() {
			c.drain++
			if during != nil {
				during()
			}
		}, func() { c.flush++ }, func() error {
			c.exec++
			return execErr
		}
}

// A signal during a restart's drain: no exec, the audit is still flushed,
// one drain (one Shutdown), and the normal shutdown path (code 0).
func TestFinishStopSignalDuringDrain(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGTERM, syscall.SIGINT} {
		stop := make(chan os.Signal, 1)
		var c stopCounts
		drain, flush, exec := c.steps(nil, func() { stop <- sig })
		if code := finishStop(true, stop, drain, flush, exec); code != 0 {
			t.Fatalf("%v: code %d, want 0", sig, code)
		}
		if c != (stopCounts{drain: 1, flush: 1, exec: 0}) {
			t.Fatalf("%v: steps %+v, want one drain, one flush, no exec", sig, c)
		}
	}
}

// A failed re-exec: one drain, the audit flushed before the exec, one
// exec attempt and a single non-zero exit code.
func TestFinishStopExecFails(t *testing.T) {
	var c stopCounts
	var order []string
	drain, _, _ := c.steps(nil, nil)
	flush := func() { c.flush++; order = append(order, "flush") }
	exec := func() error { c.exec++; order = append(order, "exec"); return errors.New("exec format error") }
	if code := finishStop(true, make(chan os.Signal, 1), drain, flush, exec); code != 1 {
		t.Fatalf("code %d, want 1", code)
	}
	if c != (stopCounts{drain: 1, flush: 1, exec: 1}) {
		t.Fatalf("steps %+v, want one of each", c)
	}
	if !reflect.DeepEqual(order, []string{"flush", "exec"}) {
		t.Fatalf("order %v, want flush before exec", order)
	}
}

// A normal shutdown only drains: no flush or exec here, code 0.
func TestFinishStopShutdown(t *testing.T) {
	stop := make(chan os.Signal, 1)
	stop <- syscall.SIGTERM
	var c stopCounts
	drain, flush, exec := c.steps(nil, nil)
	if code := finishStop(false, stop, drain, flush, exec); code != 0 {
		t.Fatalf("code %d", code)
	}
	if c != (stopCounts{drain: 1}) {
		t.Fatalf("steps %+v, want one drain only", c)
	}
}
