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
	"strings"
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
	// SessionSeconds is actual on-chain open time (ClosedAt − OpenedAt),
	// credited when a session closes with a receipt — not the configured
	// open duration.
	SessionSeconds int64 `json:"sessionSeconds,omitempty"`
}

// OpenSessionMeta tracks who opened a session so close can attribute usage.
type OpenSessionMeta struct {
	KeyID   string `json:"keyId"`
	Model   string `json:"model"`
	Tracked int64  `json:"trackedAt"` // unix when we recorded the open
}

type state struct {
	Keys            []keymaker.Record           `json:"keys"`
	Usage           map[string]*UsageRow        `json:"usage"` // "day|keyId|model"
	OpenSessions    map[string]OpenSessionMeta  `json:"openSessions,omitempty"`
	SessionTimeMode string                      `json:"sessionTimeMode,omitempty"` // "actual" after migration
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
	if s.st.OpenSessions == nil {
		s.st.OpenSessions = map[string]OpenSessionMeta{}
	}
	// One-time migrate off the old "credit full open duration on open" model.
	if s.st.SessionTimeMode != "actual" {
		for _, row := range s.st.Usage {
			row.SessionSeconds = 0
		}
		s.st.SessionTimeMode = "actual"
		_ = s.save()
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
	for _, k := range s.st.Keys {
		if k.Hash == rec.Hash {
			return fmt.Errorf("key already registered")
		}
		if k.ID == rec.ID {
			return fmt.Errorf("key id %s already exists", rec.ID)
		}
	}
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
	_ = s.save()
}

// TrackSessionOpen remembers key/model for a newly opened session so close
// can attribute actual on-chain duration.
func (s *Store) TrackSessionOpen(sessionID, keyID, model string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.OpenSessions == nil {
		s.st.OpenSessions = map[string]OpenSessionMeta{}
	}
	s.st.OpenSessions[sessionID] = OpenSessionMeta{
		KeyID:   keyID,
		Model:   model,
		Tracked: time.Now().Unix(),
	}
	_ = s.save()
}

// RecordSessionClose credits actual used seconds (from chain OpenedAt/ClosedAt)
// to the key/model that opened the session. modelFallback is used when we have
// no local open tracking (e.g. session opened before this build).
func (s *Store) RecordSessionClose(sessionID string, actualSeconds int64, modelFallback string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || actualSeconds <= 0 {
		return
	}
	day := time.Now().UTC().Format("2006-01-02")
	s.mu.Lock()
	defer s.mu.Unlock()
	keyID := "unknown"
	model := modelFallback
	if meta, ok := s.st.OpenSessions[sessionID]; ok {
		if meta.KeyID != "" {
			keyID = meta.KeyID
		}
		if meta.Model != "" {
			model = meta.Model
		}
		delete(s.st.OpenSessions, sessionID)
	}
	if model == "" {
		model = "unknown"
	}
	id := day + "|" + keyID + "|" + model
	row, ok := s.st.Usage[id]
	if !ok {
		row = &UsageRow{Day: day, KeyID: keyID, Model: model}
		s.st.Usage[id] = row
	}
	row.SessionSeconds += actualSeconds
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
