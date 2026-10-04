package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/imarchuang/marchindex/index"
)

const helpText = `marchindex — Lucene/Elasticsearch-inspired inverted index engine

Endpoints:
  GET    /healthz                      - liveness probe
  GET    /                             - this help
  PUT    /indices/{name}               - create index
  GET    /indices                      - list indices + segment counts
  POST   /indices/{name}/_doc          - index one JSON document (RAM; searchable without flush)
  POST   /indices/{name}/_flush        - freeze RAM into an immutable segment and commit it
  POST   /indices/{name}/_bulk         - NDJSON bulk (optional polish)
  DELETE /indices/{name}/_doc/{id}     - set deleted.bits; search skips until merge drops the doc
  GET    /indices/{name}/_search       - boolean search, q= and limit= (default 10)
  POST   /indices/{name}/_forcemerge   - rewrite committed segments into one
  GET    /indices/{name}/_stats        - docs, segments, terms, deletes

Query string (q), answered from committed segments and the unflushed RAM buffer:
  level:error              term in field "level"
  timeout                  term in the default field "message"
  a AND b                  intersection (AND binds tighter than OR)
  a OR b                   union
  (a OR b) AND c           parentheses
  "timeout calling"        adjacent tokens in the default field "message"
  message:"timeout db"     adjacent tokens in field "message"
NOT queries are not supported.
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

	mux.HandleFunc("POST /indices/{name}/_doc", func(w http.ResponseWriter, r *http.Request) {
		idx, ok := openIndex(w, mgr, r.PathValue("name"))
		if !ok {
			return
		}
		fields, id, err := readDocument(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		res, err := idx.IndexDocument(id, fields)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		status := http.StatusCreated
		if res.Replaced {
			status = http.StatusOK
		}
		writeJSON(w, status, res)
	})

	mux.HandleFunc("POST /indices/{name}/_flush", func(w http.ResponseWriter, r *http.Request) {
		idx, ok := openIndex(w, mgr, r.PathValue("name"))
		if !ok {
			return
		}
		res, err := idx.Flush()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
	})

	mux.HandleFunc("DELETE /indices/{name}/_doc/{id}", func(w http.ResponseWriter, r *http.Request) {
		idx, ok := openIndex(w, mgr, r.PathValue("name"))
		if !ok {
			return
		}
		res, err := idx.Delete(r.PathValue("id"))
		if err != nil {
			if errors.Is(err, index.ErrDocNotFound) {
				writeError(w, http.StatusNotFound, "document not found")
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
	})

	mux.HandleFunc("POST /indices/{name}/_forcemerge", func(w http.ResponseWriter, r *http.Request) {
		idx, ok := openIndex(w, mgr, r.PathValue("name"))
		if !ok {
			return
		}
		res, err := idx.ForceMerge()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
	})

	mux.HandleFunc("GET /indices/{name}/_search", func(w http.ResponseWriter, r *http.Request) {
		idx, ok := openIndex(w, mgr, r.PathValue("name"))
		if !ok {
			return
		}
		limit := 10
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				writeError(w, http.StatusBadRequest, "invalid limit")
				return
			}
			limit = n
		}
		res, err := idx.Search(r.URL.Query().Get("q"), limit)
		if err != nil {
			if errors.Is(err, index.ErrBadQuery) {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
	})

	return mux
}

func openIndex(w http.ResponseWriter, mgr *index.Manager, name string) (*index.Index, bool) {
	idx, err := mgr.GetIndex(name)
	if err != nil {
		if errors.Is(err, index.ErrIndexNotFound) {
			writeError(w, http.StatusNotFound, "index not found")
			return nil, false
		}
		if errors.Is(err, index.ErrInvalidIndexName) {
			writeError(w, http.StatusBadRequest, err.Error())
			return nil, false
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	return idx, true
}

func readDocument(r *http.Request) (map[string]string, string, error) {
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil, "", fmt.Errorf("invalid JSON body")
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, "", fmt.Errorf("invalid JSON body")
	}
	if raw == nil {
		return nil, "", fmt.Errorf("JSON body must be an object")
	}

	fields := make(map[string]string, len(raw))
	var id string
	for k, v := range raw {
		if v == nil {
			continue
		}
		text, err := jsonValueToText(v)
		if err != nil {
			return nil, "", err
		}
		if k == "_id" {
			id = text
			continue
		}
		fields[k] = text
	}
	return fields, id, nil
}

func jsonValueToText(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case json.Number:
		return t.String(), nil
	default:
		return "", fmt.Errorf("nested values are not supported")
	}
}
