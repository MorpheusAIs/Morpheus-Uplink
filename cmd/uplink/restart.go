package main

import (
	"fmt"
	"os"
	"syscall"
)

// restartSelf replaces this process with a fresh start of the same binary,
// with the same arguments and environment (the entrypoint has already
// dropped to the uplink user and loaded bake.env, so both carry over). The
// process id stays the same, so the container keeps running and its other
// services, the proxy-router included, are not restarted. Go opens every
// file and socket close-on-exec, so nothing leaks into the new process.
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
