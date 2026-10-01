package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/imarchuang/marchindex/index"
)

const helpText = `marchindex — Lucene/Elasticsearch-inspired inverted index engine

Endpoints:
  GET    /healthz                      - liveness probe
  GET    /                             - help text (planned endpoints)
  PUT    /indices/{name}               - create index
  GET    /indices                      - list indices + segment counts
  POST   /indices/{name}/_doc          - index one document (body JSON)
  POST   /indices/{name}/_bulk         - NDJSON bulk (optional polish)
  POST   /indices/{name}/_flush        - RAM -> new segment + commit
  DELETE /indices/{name}/_doc/{id}     - mark deleted (bitset)
  GET    /indices/{name}/_search       - q=, limit=
  POST   /indices/{name}/_forcemerge   - compact segments now
  GET    /indices/{name}/_stats        - docs, segments, terms, deletes
`

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// NewServer configures and returns the HTTP handler for marchindex.
func NewServer(mgr *index.Manager) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(helpText))
	})

	mux.HandleFunc("PUT /indices/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		_, err := mgr.CreateIndex(name)
		if err != nil {
			if errors.Is(err, index.ErrIndexExists) {
				writeError(w, http.StatusConflict, "index already exists")
				return
			}
			if errors.Is(err, index.ErrInvalidIndexName) {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		writeJSON(w, http.StatusCreated, map[string]any{
			"acknowledged": true,
			"index":        name,
		})
	})

	mux.HandleFunc("GET /indices", func(w http.ResponseWriter, r *http.Request) {
		indices, err := mgr.ListIndices()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if indices == nil {
			indices = []index.IndexInfo{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"indices": indices,
		})
	})

	return mux
}
