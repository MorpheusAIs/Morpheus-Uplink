package router

import (
	"io"
	"net/http/httptest"
	"strings"
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
		p, c := flushCopy(rec, &chunkedReader{s: routerStream, step: step})
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
	p, c := flushCopy(rec, strings.NewReader("data: {\"choices\":[]}\n\ndata: [DONE]\n\n"))
	if p != 0 || c != 0 {
		t.Fatalf("got %d/%d, want 0/0", p, c)
	}
}

func TestFlushCopyUnterminatedFinalLine(t *testing.T) {
	rec := httptest.NewRecorder()
	p, c := flushCopy(rec, strings.NewReader("data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":6}}"))
	if p != 5 || c != 6 {
		t.Fatalf("got %d/%d, want 5/6", p, c)
	}
}

func TestFlushCopySkipsOversizedLine(t *testing.T) {
	huge := "data: {" + strings.Repeat("x", maxSSELine+64) + "}\n"
	rest := "data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}\n"
	rec := httptest.NewRecorder()
	p, c := flushCopy(rec, &chunkedReader{s: huge + rest, step: 4096})
	if p != 3 || c != 4 {
		t.Fatalf("got %d/%d, want 3/4", p, c)
	}
	if !strings.Contains(rec.Body.String(), `"prompt_tokens":3`) {
		t.Fatal("usage chunk was not relayed")
	}
}
