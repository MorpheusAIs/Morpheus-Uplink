// mockrouter is a development stand-in for a proxy-router (C-Node). It
// implements just enough of the :8082 API for UPLINK to run end-to-end on a
// laptop with no wallet, no RPC, and no MOR: sessions are fake, and chat
// completions return canned (optionally streamed) responses.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	listen := envOr("MOCK_LISTEN", ":8082")
	user, pass, _ := strings.Cut(envOr("COOKIE_CONTENT", "admin:mockpass"), ":")

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
		writeJSON(w, map[string]any{"status": "healthy", "version": "mock-router", "uptime": "âˆž"})
	})

	mux.HandleFunc("GET /blockchain/balance", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"eth": uint64(42_000_000_000_000_000),     // 0.042 ETH
			"mor": uint64(12_500_000_000_000_000_000), // 12.5 MOR
		})
	}))

	mux.HandleFunc("POST /blockchain/models/{id}/session", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		id := make([]byte, 16)
		_, _ = rand.Read(id)
		session := "0x" + hex.EncodeToString(id)
		log.Printf("mock: opened session %s for model %s", session, r.PathValue("id"))
		writeJSON(w, map[string]string{"sessionID": session})
	}))

	mux.HandleFunc("POST /blockchain/sessions/{id}/close", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("mock: closed session %s", r.PathValue("id"))
		writeJSON(w, map[string]bool{"success": true})
	}))

	mux.HandleFunc("POST /v1/chat/completions", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("session_id") == "" {
			http.Error(w, `{"error":"missing session_id header"}`, http.StatusBadRequest)
			return
		}
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
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
