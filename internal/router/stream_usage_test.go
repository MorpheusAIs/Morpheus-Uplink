package router

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
)

// chunkedReader returns the stream a few bytes at a time so SSE lines
// straddle Read boundaries, as they do on a real connection.
type chunkedReader struct {
	s    string
	step int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.s == "" {
		return 0, io.EOF
	}
	n := r.step
	if n > len(r.s) {
		n = len(r.s)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, r.s[:n])
	r.s = r.s[n:]
	return n, nil
}

const routerStream = "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}],\"object\":\"chat.completion.chunk\"}\n\n" +
	"data: {\"choices\":[],\"object\":\"chat.completion.chunk\",\"usage\":{\"prompt_tokens\":27,\"completion_tokens\":60,\"total_tokens\":87},\"usage_from_provider\":{\"completion_tokens\":60,\"prompt_tokens\":10}}\n\n" +
	"data: [DONE]\n\n"

func TestFlushCopyRecordsStreamUsage(t *testing.T) {
	for _, step := range []int{1, 7, 64, 1 << 16} {
		rec := httptest.NewRecorder()
		p, c := usageOf(flushCopy(rec, &chunkedReader{s: routerStream, step: step}))
		if p != 27 || c != 60 {
			t.Fatalf("step %d: got prompt=%d completion=%d, want 27/60", step, p, c)
		}
		if rec.Body.String() != routerStream {
			t.Fatalf("step %d: stream was altered in transit", step)
		}
	}
}

func TestFlushCopyNoUsage(t *testing.T) {
	rec := httptest.NewRecorder()
	p, c := usageOf(flushCopy(rec, strings.NewReader("data: {\"choices\":[]}\n\ndata: [DONE]\n\n")))
	if p != 0 || c != 0 {
		t.Fatalf("got %d/%d, want 0/0", p, c)
	}
}

func TestFlushCopyUnterminatedFinalLine(t *testing.T) {
	rec := httptest.NewRecorder()
	p, c := usageOf(flushCopy(rec, strings.NewReader("data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":6}}")))
	if p != 5 || c != 6 {
		t.Fatalf("got %d/%d, want 5/6", p, c)
	}
}

func TestFlushCopySkipsOversizedLine(t *testing.T) {
	huge := "data: {" + strings.Repeat("x", maxSSELine+64) + "}\n"
	rest := "data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}\n"
	rec := httptest.NewRecorder()
	p, c := usageOf(flushCopy(rec, &chunkedReader{s: huge + rest, step: 4096}))
	if p != 3 || c != 4 {
		t.Fatalf("got %d/%d, want 3/4", p, c)
	}
	if !strings.Contains(rec.Body.String(), `"prompt_tokens":3`) {
		t.Fatal("usage chunk was not relayed")
	}
}

func usageOf(st streamResult) (int64, int64) { return st.promptTokens, st.completionTokens }

// A provider model error appended after the first chunk (router writes it
// as a bare JSON object) is replaced by one SSE error event and the stream
// ends; earlier chunks reach the client unchanged.
func TestFlushCopyMidStreamProviderError(t *testing.T) {
	first := "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"
	pme := `{"providerModelError":{"error":{"message":"backend unavailable"}},"statusCode":402}`
	for _, step := range []int{1, 5, 1 << 16} {
		rec := httptest.NewRecorder()
		st := flushCopy(rec, &chunkedReader{s: first + pme + "data: {\"after\":1}\n\n", step: step})
		if !st.providerError || st.providerStatus != 402 {
			t.Fatalf("step %d: providerError=%v status=%d", step, st.providerError, st.providerStatus)
		}
		got := rec.Body.String()
		if !strings.HasPrefix(got, first) {
			t.Fatalf("step %d: first chunk altered: %q", step, got)
		}
		rest := strings.TrimPrefix(got, first)
		if !strings.HasPrefix(rest, "data: {\"error\":") || !strings.HasSuffix(rest, "\n\n") || strings.Count(rest, "data:") != 1 {
			t.Fatalf("step %d: want exactly one SSE error event, got %q", step, rest)
		}
		if strings.Contains(rest, "providerModelError") || strings.Contains(rest, "backend unavailable") || strings.Contains(rest, "after") {
			t.Fatalf("step %d: raw error or later bytes relayed: %q", step, rest)
		}
		if !strings.Contains(rest, `"type":"provider_error"`) || !strings.Contains(rest, `"code":402`) {
			t.Fatalf("step %d: error event shape: %q", step, rest)
		}
	}
}

// Model output that merely mentions the word inside a data: event is not
// an error.
func TestFlushCopyProviderErrorWordInContent(t *testing.T) {
	s := "data: {\"choices\":[{\"delta\":{\"content\":\"\\\"providerModelError\\\"\"}}]}\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	st := flushCopy(rec, strings.NewReader(s))
	if st.providerError || rec.Body.String() != s {
		t.Fatalf("content mention treated as error: %+v %q", st, rec.Body.String())
	}
}

type errWriter struct {
	*httptest.ResponseRecorder
	n   int
	err error
}

func (w *errWriter) Write(b []byte) (int, error) {
	w.n++
	if w.n > 1 {
		return 0, w.err
	}
	return w.ResponseRecorder.Write(b)
}

type errReader struct {
	s    string
	done bool
	err  error
}

func (r *errReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.s), nil
	}
	return 0, r.err
}

// S4: only the upstream side of a stream is a provider fault.
func TestFlushCopyCutClassification(t *testing.T) {
	chunk := "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"
	cases := []struct {
		name string
		ctx  func() context.Context
		w    func() http.ResponseWriter
		src  io.Reader
		cut  Cut
		prov bool
	}{
		{"eof_no_done", context.Background, nil, strings.NewReader(chunk), CutUpstream, true},
		{"done", context.Background, nil, strings.NewReader(chunk + "data: [DONE]\n\n"), CutNone, false},
		{"finish", context.Background, nil, strings.NewReader(chunk + "data: {\"choices\":[{\"finish_reason\":\"length\"}]}\n\n"), CutNone, false},
		{"finish_null", context.Background, nil, strings.NewReader("data: {\"choices\":[{\"finish_reason\":null}]}\n\n"), CutUpstream, true},
		{"read_error", context.Background, nil, &errReader{s: chunk, err: errors.New("unexpected EOF")}, CutUpstream, true},
		{"client_epipe", context.Background, func() http.ResponseWriter {
			return &errWriter{ResponseRecorder: httptest.NewRecorder(), err: syscall.EPIPE}
		}, strings.NewReader(chunk + chunk), CutClient, false},
		{"client_reset", context.Background, func() http.ResponseWriter {
			return &errWriter{ResponseRecorder: httptest.NewRecorder(), err: syscall.ECONNRESET}
		}, strings.NewReader(chunk + chunk), CutClient, false},
		{"canceled", func() context.Context { c, f := context.WithCancel(context.Background()); f(); return c }, nil, &errReader{s: chunk, err: context.Canceled}, CutCanceled, false},
		{"total", func() context.Context {
			c, f := context.WithCancelCause(context.Background())
			f(ErrTotalTimeout)
			return c
		}, nil, &errReader{s: chunk, err: context.Canceled}, CutUplinkTimeout, false},
		{"idle", func() context.Context {
			c, f := context.WithCancelCause(context.Background())
			f(ErrIdleTimeout)
			return c
		}, nil, &errReader{s: chunk, err: context.Canceled}, CutUplinkTimeout, false},
		{"client_timeout", context.Background, nil, &errReader{s: chunk, err: errors.New("context deadline exceeded (Client.Timeout exceeded while reading body)")}, CutUplinkTimeout, false},
	}
	for _, c := range cases {
		var w http.ResponseWriter = httptest.NewRecorder()
		if c.w != nil {
			w = c.w()
		}
		st := flushCopyCtx(c.ctx(), w, c.src)
		if st.cut != c.cut || st.providerError != c.prov {
			t.Fatalf("%s: cut=%v provider=%v, want %v %v", c.name, st.cut, st.providerError, c.cut, c.prov)
		}
		if c.prov && st.providerStatus != 0 {
			t.Fatalf("%s: status %d, want 0 for a bare cut", c.name, st.providerStatus)
		}
	}
}
