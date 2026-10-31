// mockapi — local Servatum ingest API stand-in for manual trials.
//
//	go run ./cmd/mockapi -addr :8787 -key sv_ing_dev -token pt_dev
//	SERVATUM_ENDPOINT=http://127.0.0.1:8787 SERVATUM_KEY=sv_ing_dev servatum run --job job_x -- true
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"

	"github.com/servatum-io/servatum-cli/internal/mockapi"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	key := flag.String("key", "sv_ing_dev", "accepted ingest key")
	tok := flag.String("token", "pt_dev", "accepted ping token")
	flag.Parse()
	m := mockapi.New()
	m.Keys[*key], m.Tokens[*tok] = true, true
	mux := http.NewServeMux()
	mux.Handle("/", m)
	mux.HandleFunc("/_dump", func(w http.ResponseWriter, _ *http.Request) {
		r, p := m.Snapshot()
		_ = json.NewEncoder(w).Encode(map[string]any{"runs": r, "pings": p})
	})
	log.Printf("mock Servatum API on http://%s (key %s, token %s); GET /_dump to inspect", *addr, *key, *tok)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
