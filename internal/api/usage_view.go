package api

import (
	"sort"
	"strings"

	"github.com/absgrafx/morpheus-uplink/internal/store"
)

// UsageView is one usage row as reported by /admin/usage and /v1/usage.
// Model is the display label: the registered name plus a short id,
// "deepseek-v4-pro (0xb2c4a6)"; the short id alone for an id the catalog
// does not list; the stored text for a legacy name row that no longer
// resolves. ModelID is the full registration id when known.
type UsageView struct {
	Day              string `json:"day"`
	KeyID            string `json:"keyId"`
	Model            string `json:"model"`
	ModelID          string `json:"modelId,omitempty"`
	Requests         int64  `json:"requests"`
	PromptTokens     int64  `json:"promptTokens"`
	CompletionTokens int64  `json:"completionTokens"`
	SessionSeconds   int64  `json:"sessionSeconds,omitempty"`
}

// shortModelID is "0x" plus the first 6 hex characters.
func shortModelID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func isModelID(s string) bool { return strings.HasPrefix(strings.ToLower(s), "0x") }

// modelLabel is NameByID(id) + " (" + id[:8] + ")", or the short id alone
// when the catalog does not know the id (unknown or delisted).
func (s *Server) modelLabel(id string) string {
	if name := s.catalog.NameByID(id); name != "" {
		return name + " (" + shortModelID(id) + ")"
	}
	return shortModelID(id)
}

// usageViews resolves and merges stored rows: rows recorded by id, and
// older rows recorded by name (resolved on read when the catalog still
// has the name), roll up into one row per (day, key, registration).
func (s *Server) usageViews(rows []store.UsageRow) []UsageView {
	type k struct{ day, key, model string }
	merged := map[k]*UsageView{}
	ids := map[string]string{} // name → resolved id (per call)
	for _, r := range rows {
		id := ""
		switch m := strings.TrimSpace(r.Model); {
		case isModelID(m):
			id = strings.ToLower(m)
		case m != "" && m != "unknown":
			got, seen := ids[m]
			if !seen {
				if rid, err := s.catalog.Resolve(m); err == nil && isModelID(rid) {
					got = strings.ToLower(rid)
				}
				ids[m] = got
			}
			id = got
		}
		label := r.Model
		if label == "" {
			label = "unknown"
		}
		key := k{r.Day, r.KeyID, "name:" + label}
		if id != "" {
			key.model = id
		}
		v := merged[key]
		if v == nil {
			v = &UsageView{Day: r.Day, KeyID: r.KeyID, Model: label}
			if id != "" {
				v.ModelID = id
				v.Model = s.modelLabel(id)
			}
			merged[key] = v
		}
		v.Requests += r.Requests
		v.PromptTokens += r.PromptTokens
		v.CompletionTokens += r.CompletionTokens
		v.SessionSeconds += r.SessionSeconds
	}
	out := make([]UsageView, 0, len(merged))
	for _, v := range merged {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day > out[j].Day
		}
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		if out[i].KeyID != out[j].KeyID {
			return out[i].KeyID < out[j].KeyID
		}
		return out[i].Model < out[j].Model
	})
	return out
}
