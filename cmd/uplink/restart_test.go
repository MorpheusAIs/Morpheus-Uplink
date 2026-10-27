package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
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
