// Package auditlog keeps one persistent, rotating audit stream: a JSON
// Lines file inside DATA_DIR, appended only, rotated by rename, and pruned
// by whole rotated files once their newest line is older than the
// stream's retention. The live file is never rewritten.
package auditlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Defaults: 5 MiB per file, at most 3 files (live, .1, .2) per stream.
const (
	DefaultMaxBytes = 5 << 20
	DefaultFiles    = 3
)

// Options for one stream.
type Options struct {
	MaxBytes  int64         // rotate when the live file would pass this
	Files     int           // live + rotated files kept (>= 2)
	Retention time.Duration // rotated files whose newest line is older are deleted
}

// Stream is one audit file and its rotated copies. Safe for concurrent use.
type Stream struct {
	mu        sync.Mutex
	dir, name string
	opt       Options
	f         *os.File
	size      int64
	disabled  bool // refused at open: ring only
	writeWarn bool // a write error was logged; cleared by the next success
	now       func() time.Time
}

// Open opens (or creates) dir/name for appending. If the path is a
// symlink, not a regular file, or owned by another uid, it logs once and
// returns a disabled stream: writes are dropped and the caller keeps
// running with the in-memory ring only.
func Open(dir, name string, opt Options) *Stream {
	if opt.MaxBytes <= 0 {
		opt.MaxBytes = DefaultMaxBytes
	}
	if opt.Files < 2 {
		opt.Files = DefaultFiles
	}
	s := &Stream{dir: dir, name: name, opt: opt, now: time.Now}
	f, size, err := openSafe(s.path(0))
	if err != nil {
		slog.Warn("audit file refused; keeping the in-memory audit only", "file", name, "err", err.Error())
		s.disabled = true
		return s
	}
	s.f, s.size = f, size
	return s
}

// Disabled reports whether the stream refused its file at open.
func (s *Stream) Disabled() bool { return s.disabled }

func (s *Stream) path(i int) string {
	if i == 0 {
		return filepath.Join(s.dir, s.name)
	}
	return filepath.Join(s.dir, fmt.Sprintf("%s.%d", s.name, i))
}

var errUnsafe = errors.New("unsafe audit path")

// openSafe opens p with O_APPEND|O_CREAT|O_NOFOLLOW, mode 0600, and refuses
// a symlink, a non-regular file or a file owned by another uid.
func openSafe(p string) (*os.File, int64, error) {
	if fi, err := os.Lstat(p); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, 0, fmt.Errorf("%w: symlink", errUnsafe)
		}
		if err := checkOwned(fi); err != nil {
			return nil, 0, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	if err := checkOwned(fi); err != nil {
		f.Close()
		return nil, 0, err
	}
	if fi.Mode().Perm() != 0o600 {
		_ = f.Chmod(0o600)
	}
	return f, fi.Size(), nil
}

func checkOwned(fi os.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: not a regular file", errUnsafe)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%w: owned by another uid", errUnsafe)
	}
	return nil
}

// Append writes one JSON value as one line. A write error is logged once
// (until a later write succeeds) and is never fatal.
func (s *Stream) Append(v any) {
	line, err := json.Marshal(v)
	if err != nil {
		return
	}
	line = append(line, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled || s.f == nil {
		return
	}
	if s.size > 0 && s.size+int64(len(line)) > s.opt.MaxBytes {
		s.rotateLocked()
		if s.f == nil {
			return
		}
	}
	n, err := s.f.Write(line)
	s.size += int64(n)
	if err != nil {
		if !s.writeWarn {
			slog.Warn("audit write failed; continuing", "file", s.name, "err", err.Error())
			s.writeWarn = true
		}
		return
	}
	s.writeWarn = false
}

// rotateLocked: name.(N-2) → name.(N-1) … name → name.1, then a new live
// file. Renames stay inside the stream's directory.
func (s *Stream) rotateLocked() {
	_ = s.f.Close()
	s.f = nil
	last := s.opt.Files - 1
	_ = os.Remove(s.path(last))
	for i := last - 1; i >= 0; i-- {
		_ = os.Rename(s.path(i), s.path(i+1))
	}
	f, size, err := openSafe(s.path(0))
	if err != nil {
		slog.Warn("audit file refused after rotation; keeping the in-memory audit only", "file", s.name, "err", err.Error())
		s.disabled = true
		return
	}
	s.f, s.size = f, size
}

// Prune deletes rotated files whose newest line is older than the
// retention. The live file is never touched.
func (s *Stream) Prune() int {
	if s.opt.Retention <= 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-s.opt.Retention)
	n := 0
	for i := 1; i < s.opt.Files; i++ {
		p := s.path(i)
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		at, ok := newestAt(p)
		if !ok {
			// Nothing datable: fall back to the file time.
			at = fi.ModTime()
		}
		if at.Before(cutoff) {
			if os.Remove(p) == nil {
				n++
			}
		}
	}
	return n
}

// newestAt reads the "at" of the last complete line of a file.
func newestAt(p string) (time.Time, bool) {
	lines := tailLines(p, 1)
	if len(lines) == 0 {
		return time.Time{}, false
	}
	var v struct {
		At time.Time `json:"at"`
	}
	if json.Unmarshal(lines[0], &v) != nil || v.At.IsZero() {
		return time.Time{}, false
	}
	return v.At, true
}

// Tail returns up to n most recent lines across the rotated files and the
// live file, oldest first.
func (s *Stream) Tail(n int) [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]byte
	for i := 0; i < s.opt.Files && len(out) < n; i++ {
		fi, err := os.Lstat(s.path(i))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		got := tailLines(s.path(i), n-len(out))
		out = append(got, out...)
	}
	return out
}

// tailLines returns the last n non-empty lines of a file (at most the last
// DefaultMaxBytes*2 are read), oldest first.
func tailLines(p string, n int) [][]byte {
	if n <= 0 {
		return nil
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	var r io.Reader = f
	if fi, err := f.Stat(); err == nil && fi.Size() > 2*DefaultMaxBytes {
		_, _ = f.Seek(fi.Size()-2*DefaultMaxBytes, io.SeekStart)
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	ring := make([][]byte, 0, n)
	for sc.Scan() {
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		c := append([]byte(nil), b...)
		if len(ring) == n {
			copy(ring, ring[1:])
			ring[n-1] = c
		} else {
			ring = append(ring, c)
		}
	}
	return ring
}

// Files lists the stream's existing files (live first).
func (s *Stream) Files() []string {
	var out []string
	for i := 0; i < s.opt.Files; i++ {
		if _, err := os.Lstat(s.path(i)); err == nil {
			out = append(out, s.path(i))
		}
	}
	return out
}

// Bytes is the total size of the stream's files.
func (s *Stream) Bytes() int64 {
	var t int64
	for _, p := range s.Files() {
		if fi, err := os.Lstat(p); err == nil {
			t += fi.Size()
		}
	}
	return t
}

// Close closes the live file.
func (s *Stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// SwapFileForTest points the live handle at another path (tests simulate
// a full disk with /dev/full).
func (s *Stream) SwapFileForTest(path string) {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return
	}
	s.mu.Lock()
	if s.f != nil {
		s.f.Close()
	}
	s.f = f
	s.mu.Unlock()
}
