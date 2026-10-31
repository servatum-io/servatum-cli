// Package mockapi is a local stand-in for the Servatum ingest API, built to
// the spec. Used by tests and by `go run ./cmd/mockapi` for manual trials.
package mockapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
)

type Ping struct {
	Token, Status, Method, Body string
}

type Server struct {
	mu       sync.Mutex
	Keys     map[string]bool // valid ingest keys
	Tokens   map[string]bool // valid ping tokens
	Runs     []map[string]any
	Pings    []Ping
	seen     map[string]bool // idempotency keys
	FailNext int             // respond 503 to this many requests (for C2 tests)
}

func New() *Server {
	return &Server{Keys: map[string]bool{}, Tokens: map[string]bool{}, seen: map[string]bool{}}
}

var statuses = map[string]bool{"ok": true, "warn": true, "fail": true, "started": true}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.FailNext > 0 {
		s.FailNext--
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	switch {
	case r.URL.Path == "/v1/runs":
		s.runs(w, r)
	case strings.HasPrefix(r.URL.Path, "/p/"):
		s.ping(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	k := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !s.Keys[k] {
		http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 16*1024+1))
	if len(body) > 16*1024 {
		http.Error(w, `{"error":"too large"}`, http.StatusRequestEntityTooLarge)
		return
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	job, _ := m["job"].(string)
	st, _ := m["status"].(string)
	if job == "" || !statuses[st] {
		http.Error(w, `{"error":"job and valid status required"}`, http.StatusUnprocessableEntity)
		return
	}
	if ik, _ := m["idempotency_key"].(string); ik != "" {
		if s.seen[ik] {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"run_id":"dup","state":"up"}`))
			return
		}
		s.seen[ik] = true
	}
	s.Runs = append(s.Runs, m)
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"run_id":"run_mock","state":"up"}`))
}

func (s *Server) ping(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/p/"), "/")
	tok, st := parts[0], ""
	if len(parts) > 1 {
		st = parts[1]
	}
	if !s.Tokens[tok] {
		http.NotFound(w, r)
		return
	}
	if st != "" && st != "start" && st != "fail" {
		http.NotFound(w, r)
		return
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 10*1024+1))
	if len(b) > 10*1024 {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	s.Pings = append(s.Pings, Ping{tok, st, r.Method, string(b)})
	_, _ = w.Write([]byte("OK"))
}

func (s *Server) Snapshot() ([]map[string]any, []Ping) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.Runs...), append([]Ping(nil), s.Pings...)
}
