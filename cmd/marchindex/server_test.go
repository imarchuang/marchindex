package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imarchuang/marchindex/index"
)

func TestHealthzHandler(t *testing.T) {
	tmpDir := t.TempDir()
	mgr, err := index.NewManager(tmpDir)
	if err != nil {
		t.Fatalf("NewManager error: %v", err)
	}

	server := NewServer(mgr)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("expected status 'ok', got %q", body["status"])
	}
}

func TestRootHelpHandler(t *testing.T) {
	tmpDir := t.TempDir()
	mgr, err := index.NewManager(tmpDir)
	if err != nil {
		t.Fatalf("NewManager error: %v", err)
	}

	server := NewServer(mgr)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	body := rec.Body.String()
	expectedSubstrings := []string{
		"GET    /healthz",
		"PUT    /indices/{name}",
		"GET    /indices",
		"POST   /indices/{name}/_doc",
		"GET    /indices/{name}/_search",
		"searchable without flush",
		"default field \"message\"",
		"AND binds tighter than OR",
	}
	for _, sub := range expectedSubstrings {
		if !strings.Contains(body, sub) {
			t.Errorf("expected help text to contain %q", sub)
		}
	}
}

func TestCreateIndexHandler(t *testing.T) {
	tmpDir := t.TempDir()
	mgr, err := index.NewManager(tmpDir)
	if err != nil {
		t.Fatalf("NewManager error: %v", err)
	}
	server := NewServer(mgr)

	// 1. Successful creation
	req := httptest.NewRequest(http.MethodPut, "/indices/logs_2026", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status 201 Created, got %d: %s", rec.Code, rec.Body.String())
	}

	var createResp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&createResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if createResp["acknowledged"] != true || createResp["index"] != "logs_2026" {
		t.Fatalf("unexpected response body: %v", createResp)
	}

	// Verify on-disk layout: {dataDir}/indices/logs_2026/segments/ and segments.json
	idxDir := filepath.Join(tmpDir, "indices", "logs_2026")
	segmentsDir := filepath.Join(idxDir, "segments")
	if fi, err := os.Stat(segmentsDir); err != nil || !fi.IsDir() {
		t.Fatalf("expected segments directory to exist on disk at %s", segmentsDir)
	}

	segmentsFile := filepath.Join(idxDir, "segments.json")
	content, err := os.ReadFile(segmentsFile)
	if err != nil {
		t.Fatalf("failed to read segments.json on disk: %v", err)
	}
	var cp index.CommitPoint
	if err := json.Unmarshal(content, &cp); err != nil {
		t.Fatalf("failed to parse segments.json: %v", err)
	}
	if len(cp.Segments) != 0 {
		t.Fatalf("expected empty segments array in segments.json, got %d items", len(cp.Segments))
	}

	// 2. Duplicate creation returns 409 Conflict
	recDup := httptest.NewRecorder()
	reqDup := httptest.NewRequest(http.MethodPut, "/indices/logs_2026", nil)
	server.ServeHTTP(recDup, reqDup)

	if recDup.Code != http.StatusConflict {
		t.Fatalf("expected status 409 Conflict for duplicate index, got %d: %s", recDup.Code, recDup.Body.String())
	}

	// 3. Table-driven invalid names return 400 Bad Request
	invalidCases := []struct {
		name    string
		path    string
		expCode int
	}{
		{name: "uppercase", path: "/indices/Logs", expCode: http.StatusBadRequest},
		{name: "dot", path: "/indices/logs.v1", expCode: http.StatusBadRequest},
		{name: "symbols", path: "/indices/logs%40prod", expCode: http.StatusBadRequest},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			recInv := httptest.NewRecorder()
			reqInv := httptest.NewRequest(http.MethodPut, tc.path, nil)
			server.ServeHTTP(recInv, reqInv)
			if recInv.Code != tc.expCode {
				t.Fatalf("path %q: expected status %d, got %d: %s", tc.path, tc.expCode, recInv.Code, recInv.Body.String())
			}
		})
	}
}

func TestListIndicesHandler(t *testing.T) {
	tmpDir := t.TempDir()
	mgr, err := index.NewManager(tmpDir)
	if err != nil {
		t.Fatalf("NewManager error: %v", err)
	}
	server := NewServer(mgr)

	// Initially empty list
	req := httptest.NewRequest(http.MethodGet, "/indices", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d", rec.Code)
	}

	var listResp struct {
		Indices []index.IndexInfo `json:"indices"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&listResp); err != nil {
		t.Fatalf("failed to decode list response: %v", err)
	}
	if len(listResp.Indices) != 0 {
		t.Fatalf("expected 0 indices, got %d", len(listResp.Indices))
	}

	// Create an index
	reqPut := httptest.NewRequest(http.MethodPut, "/indices/test_index", nil)
	recPut := httptest.NewRecorder()
	server.ServeHTTP(recPut, reqPut)
	if recPut.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", recPut.Code)
	}

	// List again
	req2 := httptest.NewRequest(http.MethodGet, "/indices", nil)
	rec2 := httptest.NewRecorder()
	server.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d", rec2.Code)
	}
	var listResp2 struct {
		Indices []index.IndexInfo `json:"indices"`
	}
	if err := json.NewDecoder(rec2.Body).Decode(&listResp2); err != nil {
		t.Fatalf("failed to decode list response: %v", err)
	}
	if len(listResp2.Indices) != 1 {
		t.Fatalf("expected 1 index, got %d", len(listResp2.Indices))
	}
	if listResp2.Indices[0].Name != "test_index" {
		t.Fatalf("expected index name 'test_index', got %q", listResp2.Indices[0].Name)
	}
	if listResp2.Indices[0].SegmentCount != 0 {
		t.Fatalf("expected segment_count 0, got %d", listResp2.Indices[0].SegmentCount)
	}
}

func TestIndexAndSearchWithoutFlush(t *testing.T) {
	tmpDir := t.TempDir()
	mgr, err := index.NewManager(tmpDir)
	if err != nil {
		t.Fatalf("NewManager error: %v", err)
	}
	server := NewServer(mgr)

	req := httptest.NewRequest(http.MethodPut, "/indices/logs", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create index: status %d: %s", rec.Code, rec.Body.String())
	}

	// Documents are searchable from RAM. This test never calls _flush.
	first := postDoc(t, server, "/indices/logs/_doc", `{"service":"api","level":"error","message":"timeout calling db"}`, http.StatusCreated)
	if first.ID != "1" || first.Seq != 0 {
		t.Fatalf("first doc = %+v, want _id 1 _seq 0", first)
	}
	second := postDoc(t, server, "/indices/logs/_doc", `{"service":"api","level":"info","message":"request ok"}`, http.StatusCreated)
	if second.ID != "2" || second.Seq != 1 {
		t.Fatalf("second doc = %+v, want _id 2 _seq 1", second)
	}

	raw := searchRaw(t, server, "level:error AND service:api", "")
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode search: %v body %s", err, raw)
	}
	for _, key := range []string{"hits", "took_ms", "postings_lookups", "docs_examined"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("search response missing %s: %s", key, raw)
		}
	}

	var result struct {
		Hits            []map[string]string `json:"hits"`
		PostingsLookups int                 `json:"postings_lookups"`
		DocsExamined    int                 `json:"docs_examined"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode search struct: %v", err)
	}
	if len(result.Hits) != 1 {
		t.Fatalf("hits = %d, want 1: %s", len(result.Hits), raw)
	}
	hit := result.Hits[0]
	if hit["_id"] != "1" || hit["level"] != "error" || hit["service"] != "api" || hit["message"] != "timeout calling db" {
		t.Fatalf("hit = %#v", hit)
	}
	if result.PostingsLookups != 2 {
		t.Fatalf("postings_lookups = %d, want 2", result.PostingsLookups)
	}
	if result.DocsExamined != 1 {
		t.Fatalf("docs_examined = %d, want 1", result.DocsExamined)
	}

	segments, err := os.ReadDir(filepath.Join(tmpDir, "indices", "logs", "segments"))
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 0 {
		t.Fatalf("expected no flushed segment files, found %d", len(segments))
	}

	replaced := postDoc(t, server, "/indices/logs/_doc", `{"_id":"1","service":"api","level":"info","message":"request ok"}`, http.StatusOK)
	if replaced.ID != "1" || replaced.Seq == first.Seq {
		t.Fatalf("replacement = %+v, want same _id and a new _seq", replaced)
	}
	raw = searchRaw(t, server, "level:error", "")
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 0 {
		t.Fatalf("old terms still matched after replace: %s", raw)
	}
}

func TestNumericFieldIndexedAsText(t *testing.T) {
	tmpDir := t.TempDir()
	mgr, err := index.NewManager(tmpDir)
	if err != nil {
		t.Fatalf("NewManager error: %v", err)
	}
	server := NewServer(mgr)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/indices/logs", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	postDoc(t, server, "/indices/logs/_doc", `{"level":"error","code":500,"message":"timeout"}`, http.StatusCreated)

	raw := searchRaw(t, server, "code:500", "")
	var result struct {
		Hits []map[string]string `json:"hits"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.Hits[0]["code"] != "500" {
		t.Fatalf("numeric field search = %s", raw)
	}
}

func TestDocAndSearchErrors(t *testing.T) {
	tmpDir := t.TempDir()
	mgr, err := index.NewManager(tmpDir)
	if err != nil {
		t.Fatalf("NewManager error: %v", err)
	}
	server := NewServer(mgr)

	missing := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "doc", method: http.MethodPost, path: "/indices/missing/_doc", body: `{"level":"error"}`},
		{name: "search", method: http.MethodGet, path: "/indices/missing/_search?q=timeout"},
	}
	for _, tt := range missing {
		t.Run("missing "+tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			var body *strings.Reader
			if tt.body != "" {
				body = strings.NewReader(tt.body)
			} else {
				body = strings.NewReader("")
			}
			server.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, body))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
			}
		})
	}

	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/indices/logs", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d", rec.Code)
	}

	bad := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "invalid json", method: http.MethodPost, path: "/indices/logs/_doc", body: `{`},
		{name: "nested", method: http.MethodPost, path: "/indices/logs/_doc", body: `{"level":{"a":"b"}}`},
		{name: "bad name", method: http.MethodPost, path: "/indices/Logs/_doc", body: `{"level":"error"}`},
		{name: "phrase", method: http.MethodGet, path: "/indices/logs/_search?q=" + url.QueryEscape(`"timeout db"`)},
		{name: "not", method: http.MethodGet, path: "/indices/logs/_search?q=" + url.QueryEscape("NOT level:error")},
		{name: "bad limit", method: http.MethodGet, path: "/indices/logs/_search?q=timeout&limit=-1"},
		{name: "limit type", method: http.MethodGet, path: "/indices/logs/_search?q=timeout&limit=abc"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func postDoc(t *testing.T, server http.Handler, path, body string, want int) index.IndexResult {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	server.ServeHTTP(rec, req)
	if rec.Code != want {
		t.Fatalf("POST %s status = %d, want %d: %s", path, rec.Code, want, rec.Body.String())
	}
	var res index.IndexResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode index response: %v", err)
	}
	return res
}

func searchRaw(t *testing.T, server http.Handler, q, limit string) []byte {
	t.Helper()
	path := "/indices/logs/_search?q=" + url.QueryEscape(q)
	if limit != "" {
		path += "&limit=" + url.QueryEscape(limit)
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("search status = %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}
