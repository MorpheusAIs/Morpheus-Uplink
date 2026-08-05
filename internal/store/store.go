// Package store persists API key records and usage counters to a single
// JSON file with atomic writes. State is treated as disposable: losing it
// revokes generated keys and forgets usage history, but the master key and
// all session/wallet state live elsewhere (env seed and the chain).
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/absgrafx/morpheus-uplink/internal/keymaker"
)

type UsageRow struct {
	Day              string `json:"day"` // YYYY-MM-DD (UTC)
	KeyID            string `json:"keyId"`
	Model            string `json:"model"`
	Requests         int64  `json:"requests"`
	PromptTokens     int64  `json:"promptTokens"`
	CompletionTokens int64  `json:"completionTokens"`
}

type state struct {
	Keys  []keymaker.Record        `json:"keys"`
	Usage map[string]*UsageRow `json:"usage"` // "day|keyId|model"
}

type Store struct {
	mu   sync.Mutex
	path string
	st   state
}

func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	s := &Store{
		path: filepath.Join(dataDir, "uplink-state.json"),
		st:   state{Usage: map[string]*UsageRow{}},
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(raw, &s.st); err != nil {
		return nil, fmt.Errorf("parse state file %s: %w", s.path, err)
	}
	if s.st.Usage == nil {
		s.st.Usage = map[string]*UsageRow{}
	}
	return s, nil
}

// save must be called with s.mu held.
func (s *Store) save() error {
	raw, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) AddKey(rec keymaker.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Keys = append(s.st.Keys, rec)
	return s.save()
}

func (s *Store) RevokeKey(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.st.Keys {
		if k.ID == id {
			s.st.Keys = append(s.st.Keys[:i], s.st.Keys[i+1:]...)
			return true, s.save()
		}
	}
	return false, nil
}

func (s *Store) ListKeys() []keymaker.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]keymaker.Record, len(s.st.Keys))
	copy(out, s.st.Keys)
	return out
}

// LookupKey returns the key ID for a presented full key, or "" if unknown.
func (s *Store) LookupKey(full string) string {
	h := keymaker.Hash(full)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.st.Keys {
		if k.Hash == h {
			return k.ID
		}
	}
	return ""
}

func (s *Store) RecordUsage(keyID, model string, promptTokens, completionTokens int64) {
	day := time.Now().UTC().Format("2006-01-02")
	id := day + "|" + keyID + "|" + model
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.st.Usage[id]
	if !ok {
		row = &UsageRow{Day: day, KeyID: keyID, Model: model}
		s.st.Usage[id] = row
	}
	row.Requests++
	row.PromptTokens += promptTokens
	row.CompletionTokens += completionTokens
	// Usage is best-effort; a failed save is not worth failing a request.
	_ = s.save()
}

func (s *Store) Usage() []UsageRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]UsageRow, 0, len(s.st.Usage))
	for _, r := range s.st.Usage {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day > out[j].Day
		}
		return out[i].Requests > out[j].Requests
	})
	return out
}
