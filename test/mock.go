//go:build ignore

// Test helper: a tiny HTTP server that mimics an OpenAI endpoint, returning
// configurable HTTP status codes so we can verify the proxy's retry/failover
// behaviour without a real API key.
//
// Run:   go run test/mock.go
// Behavior:
//   GET /mode            -> returns current mode (ok | 429 | 401)
//   POST /mode           -> set mode (e.g. {"mode":"429"}), affects next calls
//   POST /v1/chat/completions -> echoes back the request verbatim + status by mode
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	port := os.Getenv("MOCK_PORT")
	if port == "" {
		port = "9100"
	}
	mode := "ok"
	mux := http.NewServeMux()
	mux.HandleFunc("/mode", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var in struct {
				Mode string `json:"mode"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			if in.Mode != "" {
				mode = in.Mode
			}
		}
		json.NewEncoder(w).Encode(map[string]string{"mode": mode})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		// Streaming branch: if the request asks for stream, emit SSE chunks with
		// a delay between them so a buffering proxy would be detectable.
		var req struct {
			Stream bool `json:"stream"`
		}
		json.Unmarshal(body, &req)
		if req.Stream && mode == "ok" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			flusher, _ := w.(http.Flusher)
			for i := 1; i <= 3; i++ {
				fmt.Fprintf(w, "data: {\"chunk\":%d,\"ts\":%d}\n\n", i, time.Now().UnixMilli())
				if flusher != nil {
					flusher.Flush()
				}
				time.Sleep(400 * time.Millisecond)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}

		switch mode {
		case "429":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
		case "401":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"message":"bad key","type":"invalid_request_error"}}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"mode":"%s","echo":%s,"auth":"%s"}`, mode, body, r.Header.Get("Authorization"))
		}
	})
	log.Printf("mock listening on :%s (mode=%s)", port, mode)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
