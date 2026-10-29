// mockrouter is a development stand-in for a proxy-router (C-Node). It
// implements just enough of the :8082 API for UPLINK to run end-to-end on a
// laptop with no wallet, no RPC, and no MOR: sessions are fake, and chat
// completions return canned (optionally streamed) responses.
//
// It listens on loopback (127.0.0.1:8082 by default) and also serves the two
// gateway feeds for its own fake bids, so a local UPLINK can use:
//
//	ROUTER_URL=http://127.0.0.1:8082
//	ACTIVE_MODELS_URL=http://127.0.0.1:8082/gateway_models.json
//	GATEWAY_BIDS_URL=http://127.0.0.1:8082/gateway_bids.json
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// mockModels are the fake models; each has one healthy bid from
// mockProvider (bid id: bidOf).
var mockModels = []struct{ ID, Name string }{
	{"0x" + strings.Repeat("a0", 32), "example-model"},
	{"0x" + strings.Repeat("a1", 32), "example-embed"},
}

// mockProvider is written in mixed case, as the router writes addresses.
const mockProvider = "0xAbAbAbAbAbAbAbAbAbAbAbAbAbAbAbAbAbAbAbAb"

// bidOf is the fake bid id of a model.
func bidOf(model string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(model)))
	return "0x" + hex.EncodeToString(sum[:])
}

type mockSession struct {
	Id           string
	ModelAgentId string
	Stake        string
	EndsAt       int64
	ClosedAt     int64
}

func main() {
	listen := envOr("MOCK_LISTEN", "127.0.0.1:8082")
	user, pass, _ := strings.Cut(envOr("COOKIE_CONTENT", "admin:localdev"), ":")

	var mu sync.Mutex
	open := map[string]mockSession{}

	requireAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			if !ok || u != user || p != pass {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthcheck", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"status": "healthy", "version": "mock-router", "uptime": "∞"})
	})

	mux.HandleFunc("GET /wallet", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"address": "0x1111111111111111111111111111111111111111"})
	}))

	mux.HandleFunc("GET /blockchain/balance", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"eth": "42000000000000000",    // 0.042 ETH
			"mor": "12500000000000000000", // 12.5 MOR
		})
	}))

	mux.HandleFunc("GET /blockchain/sessions/user", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		list := make([]mockSession, 0, len(open))
		for _, s := range open {
			list = append(list, s)
		}
		mu.Unlock()
		writeJSON(w, map[string]any{"sessions": list})
	}))

	openSession := func(w http.ResponseWriter, model string) {
		id := make([]byte, 16)
		_, _ = rand.Read(id)
		session := "0x" + hex.EncodeToString(id)
		mu.Lock()
		open[session] = mockSession{
			Id:           session,
			ModelAgentId: model,
			Stake:        "1000000000000000000",
			EndsAt:       time.Now().Add(time.Hour).Unix(),
			ClosedAt:     0,
		}
		mu.Unlock()
		log.Printf("mock: opened session %s for model %s", session, model)
		writeJSON(w, map[string]string{"sessionID": session})
	}

	mux.HandleFunc("POST /blockchain/models/{id}/session", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		openSession(w, r.PathValue("id"))
	}))

	// Open by bid: the bid must be one of the fake bids.
	mux.HandleFunc("POST /blockchain/bids/{id}/session", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		bid := strings.ToLower(r.PathValue("id"))
		for _, m := range mockModels {
			if bidOf(m.ID) == bid {
				openSession(w, m.ID)
				return
			}
		}
		http.Error(w, `{"error":"bid not found"}`, http.StatusNotFound)
	}))

	// Rated bids in the router's shape: the outer ID is left unset (zero
	// hash) and the bid id is Bid.Id. One fake provider per model.
	mux.HandleFunc("GET /blockchain/models/{id}/bids/rated", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		model := r.PathValue("id")
		writeJSON(w, map[string]any{"bids": []map[string]any{{
			"ID": "0x" + strings.Repeat("0", 64),
			"Bid": map[string]any{
				"Id": bidOf(model), "Provider": mockProvider, "ModelAgentId": model,
				"PricePerSecond": "1000", "Nonce": "0", "CreatedAt": "1700000000", "DeletedAt": "0",
			},
			"Score": 1.0,
		}}})
	}))

	// Gateway feeds (no auth, like the public gateway) for the fake bids.
	mux.HandleFunc("GET /gateway_bids.json", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().Unix()
		utc := time.Unix(now, 0).UTC().Format(time.RFC3339)
		rows := []map[string]any{}
		for _, m := range mockModels {
			rows = append(rows, map[string]any{
				"Id": bidOf(m.ID), "Provider": strings.ToLower(mockProvider), "ModelAgentId": m.ID,
				"PricePerSecond": "1000", "Nonce": "0", "CreatedAt": "1700000000", "DeletedAt": "0", "ModelName": m.Name,
				"health": map[string]any{
					"status": "healthy", "errorKind": "", "httpStatus": 200, "latencyMs": 100, "promptCorrect": true,
					"providerVersion": "mock", "providerPingMs": 5,
					"lastHealthy": now, "lastHealthyUTC": utc, "lastChecked": now, "lastCheckedUTC": utc,
				},
			})
		}
		writeJSON(w, map[string]any{"last_updated": now, "last_updated_utc": utc, "bids": rows})
	})

	mux.HandleFunc("GET /gateway_models.json", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().Unix()
		models := []map[string]any{}
		for _, m := range mockModels {
			models = append(models, map[string]any{
				"Id": m.ID, "Name": m.Name, "Tags": []string{}, "ModelType": "LLM",
				"bidDetail": []map[string]any{{
					"bidId": bidOf(m.ID), "providerId": strings.ToLower(mockProvider),
					"pricePerSecond": "1000", "priceMorPerHour": 0.0000036, "status": "healthy",
				}},
			})
		}
		writeJSON(w, map[string]any{"last_updated": now, "last_updated_utc": time.Unix(now, 0).UTC().Format(time.RFC3339), "models": models})
	})

	mux.HandleFunc("POST /blockchain/sessions/{id}/close", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		sid := r.PathValue("id")
		mu.Lock()
		delete(open, sid)
		mu.Unlock()
		log.Printf("mock: closed session %s", sid)
		writeJSON(w, map[string]bool{"success": true})
	}))

	mux.HandleFunc("POST /v1/chat/completions", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("session_id") == "" {
			http.Error(w, `{"error":"missing session_id header"}`, http.StatusBadRequest)
			return
		}
		var req struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		last := "(empty)"
		if n := len(req.Messages); n > 0 {
			last = req.Messages[n-1].Content
		}
		reply := fmt.Sprintf("[mock %s via session %s…] You said: %q. This is a canned response — swap ROUTER_URL to a real C-Node for live Morpheus inference.",
			req.Model, r.Header.Get("session_id")[:10], last)

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			for _, word := range strings.Fields(reply) {
				chunk := map[string]any{
					"object": "chat.completion.chunk",
					"model":  req.Model,
					"choices": []any{map[string]any{
						"delta": map[string]string{"content": word + " "},
					}},
				}
				raw, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", raw)
				flusher.Flush()
				time.Sleep(40 * time.Millisecond)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		writeJSON(w, map[string]any{
			"object": "chat.completion",
			"model":  req.Model,
			"choices": []any{map[string]any{
				"index":         0,
				"finish_reason": "stop",
				"message":       map[string]string{"role": "assistant", "content": reply},
			}},
			"usage": map[string]int{"prompt_tokens": len(last) / 4, "completion_tokens": len(reply) / 4},
		})
	}))

	mux.HandleFunc("POST /v1/embeddings", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		vec := make([]float64, 8)
		for i := range vec {
			vec[i] = float64(i) * 0.1
		}
		writeJSON(w, map[string]any{
			"object": "list",
			"data":   []any{map[string]any{"object": "embedding", "index": 0, "embedding": vec}},
			"usage":  map[string]int{"prompt_tokens": 5, "completion_tokens": 0},
		})
	}))

	log.Printf("mock router listening on %s (auth %s:***)", listen, user)
	log.Fatal(http.ListenAndServe(listen, mux))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
