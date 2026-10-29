package pool

import (
	"bufio"
	"os"
	"strings"
	"testing"
	"time"
)

// Every 429 text captured from the router's own open path classifies as
// recorded (one RPC URL, window retry unused, 1s POST). A window-stage
// text is maybe-sent when the 18 s rule may not be used.
func TestClassifierRouter429Captures(t *testing.T) {
	f, err := os.Open("testdata/router-v7.18.0-rpc429-captures.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			t.Fatalf("bad line %q", line)
		}
		step, want, text := parts[0], parts[1], parts[2]
		n++
		if got := classifyOpenErr(status500(text), time.Second, true); got.String() != want {
			t.Errorf("%s: %s, want %s (%s)", step, got, want, text)
		}
		if want == transientWindow.String() {
			if got := classifyOpenErr(status500(text), time.Second, false); got != maybeSent {
				t.Errorf("%s without the window rule: %s, want maybe_sent", step, got)
			}
			if got := classifyOpenErr(status500(text), preSendProofWindow, true); got != maybeSent {
				t.Errorf("%s at the proof window: %s, want maybe_sent", step, got)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 8 {
		t.Fatalf("%d captures, want every step", n)
	}
}
