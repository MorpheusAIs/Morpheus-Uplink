package main

import (
	"fmt"
	"log"
	"os"
	"syscall"
)

// restartSelf replaces this process with a fresh start of the same binary,
// with the same arguments and environment (the entrypoint has already
// dropped to the uplink user and loaded bake.env, so both carry over). The
// process id stays the same, so Uplink's container keeps running; the
// proxy-router is a separate compose service in its own container and is
// not restarted. Go opens every file and socket close-on-exec, so nothing
// leaks into the new process.
// It returns only on failure.
func restartSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}
	return restartWith(execSelf, exe, os.Args, os.Environ())
}

// restartWith runs exec with argv[0] kept as given; split out for tests.
func restartWith(exec func(path string, argv, env []string) error, exe string, argv, env []string) error {
	if len(argv) == 0 {
		argv = []string{exe}
	}
	if err := exec(exe, argv, env); err != nil {
		return fmt.Errorf("exec %s: %w", exe, err)
	}
	return nil
}

// execSelf is syscall.Exec (Uplink builds for Linux and other Unix only).
var execSelf = syscall.Exec

// drainThenDecide runs drain (the graceful HTTP stop) and reports whether
// the process should still start again in place. A stop signal that
// arrives while a restart drains, or before the restart starts, cancels
// the restart: the drain still finishes and the caller takes the normal
// shutdown path, exit 0. Without a restart, it only waits for the drain.
func drainThenDecide(restart bool, stop <-chan os.Signal, drain func()) bool {
	done := make(chan struct{})
	go func() {
		drain()
		close(done)
	}()
	for {
		select {
		case <-done:
			if restart {
				select {
				case sig := <-stop:
					log.Printf("restart cancelled: %s received; shutting down", sig)
					return false
				default:
				}
			}
			return restart
		case sig := <-stop:
			if restart {
				log.Printf("restart cancelled: %s received while draining; shutting down", sig)
				restart = false
			}
		}
	}
}

// finishStop runs the stop sequence once the main loop has chosen a
// restart or a shutdown. drain (the graceful HTTP stop) runs exactly once.
// After a restart request, flush (pending audit summaries) runs once,
// whether the restart goes ahead or a stop signal cancelled it. It returns
// 0 when the caller should take the normal shutdown path and return, and
// 1 when the re-exec failed and the caller should exit non-zero. On a
// successful re-exec it does not return. Nothing is shut down or closed a
// second time.
func finishStop(restart bool, stop <-chan os.Signal, drain, flush func(), exec func() error) int {
	reexec := drainThenDecide(restart, stop, drain)
	if !restart {
		return 0
	}
	flush()
	if !reexec {
		return 0
	}
	err := exec()
	log.Printf("restart: %v; exiting so the container restart policy starts Uplink again", err)
	return 1
}
