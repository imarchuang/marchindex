package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
