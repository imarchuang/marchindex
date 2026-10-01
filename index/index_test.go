package index

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateIndexName(t *testing.T) {
	tests := []struct {
		name    string
		idxName string
		wantErr bool
	}{
		{name: "simple valid", idxName: "logs", wantErr: false},
		{name: "with underscore", idxName: "my_index", wantErr: false},
		{name: "with hyphen", idxName: "index-123", wantErr: false},
		{name: "combined", idxName: "app_log-2026", wantErr: false},
		{name: "numeric", idxName: "0123", wantErr: false},
		{name: "empty", idxName: "", wantErr: true},
		{name: "uppercase", idxName: "Logs", wantErr: true},
		{name: "space", idxName: "my index", wantErr: true},
		{name: "slash", idxName: "logs/api", wantErr: true},
		{name: "backslash", idxName: "logs\\api", wantErr: true},
		{name: "dot", idxName: ".", wantErr: true},
		{name: "dot dot", idxName: "..", wantErr: true},
		{name: "path traversal prefix", idxName: "../logs", wantErr: true},
		{name: "path traversal inside", idxName: "foo/../bar", wantErr: true},
		{name: "special chars", idxName: "logs@api", wantErr: true},
		{name: "punctuation dot", idxName: "logs.prod", wantErr: true},
		{name: "too long", idxName: strings.Repeat("a", 256), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateIndexName(tt.idxName)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateIndexName(%q) error = %v, wantErr %v", tt.idxName, err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, ErrInvalidIndexName) {
				t.Fatalf("expected ErrInvalidIndexName, got %v", err)
			}
		})
	}
}

func TestCreateIndex(t *testing.T) {
	tmpDir := t.TempDir()
	mgr, err := NewManager(tmpDir)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	idx, err := mgr.CreateIndex("logs")
	if err != nil {
		t.Fatalf("CreateIndex failed: %v", err)
	}
	if idx.Name() != "logs" {
		t.Fatalf("expected index name 'logs', got %q", idx.Name())
	}

	// Verify segments/ directory exists
	segDirInfo, err := os.Stat(idx.SegmentsDir())
	if err != nil || !segDirInfo.IsDir() {
		t.Fatalf("expected segments directory to exist at %s", idx.SegmentsDir())
	}

	// Verify segments.json exists and contains empty segments array
	content, err := os.ReadFile(idx.SegmentsFilePath())
	if err != nil {
		t.Fatalf("failed to read segments.json: %v", err)
	}

	var cp CommitPoint
	if err := json.Unmarshal(content, &cp); err != nil {
		t.Fatalf("failed to unmarshal segments.json: %v", err)
	}
	if len(cp.Segments) != 0 {
		t.Fatalf("expected 0 segments, got %d", len(cp.Segments))
	}

	// Attempt duplicate creation -> should return ErrIndexExists
	_, err = mgr.CreateIndex("logs")
	if !errors.Is(err, ErrIndexExists) {
		t.Fatalf("expected ErrIndexExists on duplicate create, got %v", err)
	}

	// Attempt invalid name -> should return ErrInvalidIndexName
	_, err = mgr.CreateIndex("../bad")
	if !errors.Is(err, ErrInvalidIndexName) {
		t.Fatalf("expected ErrInvalidIndexName on invalid name, got %v", err)
	}
}

func TestListIndices(t *testing.T) {
	tmpDir := t.TempDir()
	mgr, err := NewManager(tmpDir)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	// Empty list initially
	list, err := mgr.ListIndices()
	if err != nil {
		t.Fatalf("ListIndices failed: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 indices, got %d", len(list))
	}

	// Create multiple indices in non-alphabetical order
	names := []string{"logs_b", "logs_a", "logs_c"}
	for _, name := range names {
		if _, err := mgr.CreateIndex(name); err != nil {
			t.Fatalf("failed to create index %q: %v", name, err)
		}
	}

	// Verify listed and sorted
	list, err = mgr.ListIndices()
	if err != nil {
		t.Fatalf("ListIndices failed: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 indices, got %d", len(list))
	}

	expected := []string{"logs_a", "logs_b", "logs_c"}
	for i, exp := range expected {
		if list[i].Name != exp {
			t.Errorf("list[%d].Name = %q, want %q", i, list[i].Name, exp)
		}
		if list[i].SegmentCount != 0 {
			t.Errorf("list[%d].SegmentCount = %d, want 0", i, list[i].SegmentCount)
		}
	}
}

func TestGetIndex(t *testing.T) {
	tmpDir := t.TempDir()
	mgr, err := NewManager(tmpDir)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	// Not found initially
	_, err = mgr.GetIndex("nonexistent")
	if !errors.Is(err, ErrIndexNotFound) {
		t.Fatalf("expected ErrIndexNotFound, got %v", err)
	}

	// Create and get
	_, err = mgr.CreateIndex("my_index")
	if err != nil {
		t.Fatalf("CreateIndex failed: %v", err)
	}

	idx, err := mgr.GetIndex("my_index")
	if err != nil {
		t.Fatalf("GetIndex failed: %v", err)
	}
	if idx.Name() != "my_index" {
		t.Fatalf("expected index name 'my_index', got %q", idx.Name())
	}
	expectedBaseDir := filepath.Join(tmpDir, "indices", "my_index")
	if idx.BaseDir() != expectedBaseDir {
		t.Fatalf("expected baseDir %q, got %q", expectedBaseDir, idx.BaseDir())
	}
}
