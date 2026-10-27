package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

// S5-4: admin calls time out at 60 s on their own client; inference has
// no client timeout (its own timers bound it).
func TestS5AdminClientSeparate(t *testing.T) {
	c := New("http://x", "u", "p")
	if c.HTTP.Timeout != 60*time.Second || AdminTimeout != 60*time.Second {
		t.Fatalf("admin timeout %v", c.HTTP.Timeout)
	}
	if c.ForwardHTTP == nil || c.ForwardHTTP.Timeout != 0 || c.ForwardHTTP == c.HTTP {
		t.Fatal("forward client must be separate with no Client.Timeout")
	}
	// Functional: a hung admin call ends at the admin timeout while a
	// forward on the same client keeps streaming past it.
	hang := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			w.Header().Set("Content-Type", "text/event-stream")
			for i := 0; i < 4; i++ {
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
				w.(http.Flusher).Flush()
				time.Sleep(40 * time.Millisecond)
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	defer ts.Close()
	defer close(hang)
	c = New(ts.URL, "u", "p")
	c.HTTP.Timeout = 50 * time.Millisecond
	start := time.Now()
	if _, err := c.Balance(); err == nil || time.Since(start) > time.Second {
		t.Fatalf("admin call err=%v after %v", err, time.Since(start))
	}
	rec := httptest.NewRecorder()
	res, err := c.ForwardWith(context.Background(), rec, "/v1/chat/completions", "s", http.Header{}, []byte(`{}`), false, ForwardOptions{Idle: time.Second, Total: 5 * time.Second, Stream: true})
	if err != nil || res.Cut != CutNone || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("forward err=%v res=%+v", err, res)
	}
}

func sseServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *Client {
	ts := httptest.NewServer(http.HandlerFunc(h))
	t.Cleanup(ts.Close)
	return New(ts.URL, "u", "p")
}

func chunk(w http.ResponseWriter, s string) {
	_, _ = io.WriteString(w, s)
	w.(http.Flusher).Flush()
}

// S5-1: bytes dribbling under the idle timer complete; silence past it
// cuts with the error event, the uplink_idle_timeout finish chunk and [DONE].
func TestS5IdleTimerStream(t *testing.T) {
	const u = 2 * time.Millisecond // 1 s of the spec = 2 ms here
	c := sseServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if r.Header.Get("session_id") == "dribble" {
			// One byte per 100 s for 15 min.
			line := "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"
			for i := 0; i < 9; i++ {
				for j := 0; j < len(line); j += len(line)/5 + 1 {
					chunk(w, line[j:min(j+len(line)/5+1, len(line))])
					time.Sleep(100 * u / 5)
				}
			}
			chunk(w, "data: [DONE]\n\n")
			return
		}
		chunk(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		select { // silent
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	rec := httptest.NewRecorder()
	res, err := c.ForwardWith(context.Background(), rec, "/v1/chat/completions", "dribble", http.Header{}, []byte(`{}`), false,
		ForwardOptions{Idle: 180 * u, Total: 1000 * u, Stream: true})
	if err != nil || res.Cut != CutNone || !strings.HasSuffix(rec.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("dribble: err=%v res=%+v", err, res)
	}
	rec = httptest.NewRecorder()
	start := time.Now()
	res, err = c.ForwardWith(context.Background(), rec, "/v1/chat/completions", "silent", http.Header{}, []byte(`{}`), false,
		ForwardOptions{Idle: 180 * u, Total: 1000 * u, Stream: true})
	if err != nil || res.Cut != CutUplinkTimeout || res.TimeoutKind != "idle" || res.MidStreamProviderError {
		t.Fatalf("silent: err=%v res=%+v", err, res)
	}
	if el := time.Since(start); el > 900*u {
		t.Fatalf("idle cut after %v", el)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"timeout"`) || !strings.Contains(body, `"finish_reason":"uplink_idle_timeout"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("tail %q", body)
	}
	// Total: bytes keep flowing but the total ends it.
	c2 := sseServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * u):
				chunk(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
			}
		}
	})
	rec = httptest.NewRecorder()
	res, err = c2.ForwardWith(context.Background(), rec, "/v1/chat/completions", "s", http.Header{}, []byte(`{}`), false,
		ForwardOptions{Idle: 180 * u, Total: 100 * u, Stream: true})
	if err != nil || res.TimeoutKind != "total" || !strings.Contains(rec.Body.String(), `"finish_reason":"uplink_total_timeout"`) {
		t.Fatalf("total: err=%v res=%+v", err, res)
	}
}

// S5-1/2: before headers the idle timer gives a TimeoutError (the caller
// answers 504); a non-stream body is not idle-cut after headers.
func TestS5IdleBeforeHeadersAndNonStream(t *testing.T) {
	const u = 2 * time.Millisecond
	c := sseServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("session_id") == "noheaders" {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(300 * u) // longer than idle, shorter than total
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	rec := httptest.NewRecorder()
	_, err := c.ForwardWith(context.Background(), rec, "/v1/chat/completions", "noheaders", http.Header{}, []byte(`{}`), true,
		ForwardOptions{Idle: 50 * u, Total: 1000 * u})
	te, ok := err.(*TimeoutError)
	if !ok || te.Kind != "idle" || rec.Body.Len() != 0 {
		t.Fatalf("want idle TimeoutError, got %v body %q", err, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	res, err := c.ForwardWith(context.Background(), rec, "/v1/chat/completions", "slowbody", http.Header{}, []byte(`{}`), true,
		ForwardOptions{Idle: 50 * u, Total: 1000 * u})
	if err != nil || res.StatusCode != 200 || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("non-stream: err=%v body %q", err, rec.Body.String())
	}
}

// S5-3: a client disconnect cancels the upstream request within 1 s.
func TestS5ClientCancelReachesUpstream(t *testing.T) {
	seen := make(chan time.Time, 1)
	c := sseServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		<-r.Context().Done()
		seen <- time.Now()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	res, _ := c.ForwardWith(ctx, httptest.NewRecorder(), "/v1/chat/completions", "s", http.Header{}, []byte(`{}`), false, ForwardOptions{Idle: time.Minute, Total: time.Minute, Stream: true})
	canceledAt := time.Now()
	select {
	case at := <-seen:
		if at.Sub(canceledAt) > time.Second {
			t.Fatal("upstream saw the cancel late")
		}
	case <-time.After(time.Second):
		t.Fatal("upstream never saw the cancel")
	}
	if res == nil || res.Cut != CutCanceled {
		t.Fatalf("res %+v", res)
	}
}

// S5-23: timers end with their request: goroutines flat after 1,000 calls
// (normal, cut and cancelled).
func TestS5TimerGoroutinesFlat(t *testing.T) {
	c := sseServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch r.Header.Get("session_id") {
		case "cut":
			chunk(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
			<-r.Context().Done()
		default:
			chunk(w, "data: [DONE]\n\n")
		}
	})
	c.ForwardHTTP.Transport = &http.Transport{MaxIdleConnsPerHost: 4}
	run := func(i int) {
		kind := []string{"ok", "cut", "cancel"}[i%3]
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		o := ForwardOptions{Idle: time.Minute, Total: time.Minute, Stream: true}
		if kind == "cut" {
			o.Idle = 2 * time.Millisecond
		}
		if kind == "cancel" {
			cancel()
		}
		_, _ = c.ForwardWith(ctx, httptest.NewRecorder(), "/v1/chat/completions", kind, http.Header{}, []byte(`{}`), false, o)
	}
	for i := 0; i < 30; i++ {
		run(i)
	}
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()
	for i := 0; i < 1000; i++ {
		run(i)
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base+10 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base+10 {
		t.Fatalf("goroutines %d → %d after 1000 calls", base, n)
	}
}
