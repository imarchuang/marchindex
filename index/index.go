package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var (
	// ErrInvalidIndexName is returned when an index name fails validation.
	ErrInvalidIndexName = errors.New("invalid index name")
	// ErrIndexExists is returned when attempting to create an index that already exists.
	ErrIndexExists = errors.New("index already exists")
	// ErrIndexNotFound is returned when an index does not exist.
	ErrIndexNotFound = errors.New("index not found")

	validNamePattern = regexp.MustCompile(`^[a-z0-9_-]+$`)
)

// ValidateIndexName validates that an index name contains only lowercase alphanumeric
// characters, underscores, and hyphens, and rejects any path traversal characters.
func ValidateIndexName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: name cannot be empty", ErrInvalidIndexName)
	}
	if len(name) > 255 {
		return fmt.Errorf("%w: name exceeds 255 characters", ErrInvalidIndexName)
	}
	if strings.Contains(name, "..") || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("%w: path traversal characters are not allowed", ErrInvalidIndexName)
	}
	if !validNamePattern.MatchString(name) {
		return fmt.Errorf("%w: must consist of lowercase alphanumeric characters, underscores, and hyphens", ErrInvalidIndexName)
	}
	return nil
}

// CommitPoint represents the contents of segments.json, the active commit point.
type CommitPoint struct {
	Segments []any `json:"segments"`
}

// IndexInfo provides basic metadata for listing indices.
type IndexInfo struct {
	Name         string `json:"name"`
	SegmentCount int    `json:"segment_count"`
}

// Index represents an index on disk.
type Index struct {
	name    string
	baseDir string
}

// Name returns the index name.
func (idx *Index) Name() string {
	return idx.name
}

// BaseDir returns the root directory for this index.
func (idx *Index) BaseDir() string {
	return idx.baseDir
}

// SegmentsDir returns the segments subdirectory.
func (idx *Index) SegmentsDir() string {
	return filepath.Join(idx.baseDir, "segments")
}

// SegmentsFilePath returns the path to segments.json.
func (idx *Index) SegmentsFilePath() string {
	return filepath.Join(idx.baseDir, "segments.json")
}

// Manager manages indices stored in dataDir.
type Manager struct {
	dataDir string
	mu      sync.RWMutex
}

// NewManager creates a new Manager instance pointing to dataDir.
func NewManager(dataDir string) (*Manager, error) {
	indicesDir := filepath.Join(dataDir, "indices")
	if err := os.MkdirAll(indicesDir, 0755); err != nil {
		return nil, fmt.Errorf("create indices root dir: %w", err)
	}
	return &Manager{
		dataDir: dataDir,
	}, nil
}

// DataDir returns the root data directory.
func (m *Manager) DataDir() string {
	return m.dataDir
}

// CreateIndex creates an empty index directory with segments/ and an empty segments.json.
func (m *Manager) CreateIndex(name string) (*Index, error) {
	if err := ValidateIndexName(name); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	idxDir := filepath.Join(m.dataDir, "indices", name)
	if _, err := os.Stat(idxDir); err == nil {
		return nil, ErrIndexExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat index directory: %w", err)
	}

	segmentsDir := filepath.Join(idxDir, "segments")
	if err := os.MkdirAll(segmentsDir, 0755); err != nil {
		return nil, fmt.Errorf("create segments directory: %w", err)
	}

	commitPath := filepath.Join(idxDir, "segments.json")
	tmpPath := filepath.Join(idxDir, "segments.json.tmp")

	cp := CommitPoint{Segments: []any{}}
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		_ = os.RemoveAll(idxDir)
		return nil, fmt.Errorf("marshal segments.json: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		_ = os.RemoveAll(idxDir)
		return nil, fmt.Errorf("write temp segments.json: %w", err)
	}

	if err := os.Rename(tmpPath, commitPath); err != nil {
		_ = os.RemoveAll(idxDir)
		return nil, fmt.Errorf("commit segments.json: %w", err)
	}

	return &Index{
		name:    name,
		baseDir: idxDir,
	}, nil
}

// GetIndex retrieves an existing index by name.
func (m *Manager) GetIndex(name string) (*Index, error) {
	if err := ValidateIndexName(name); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	idxDir := filepath.Join(m.dataDir, "indices", name)
	fi, err := os.Stat(idxDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrIndexNotFound
		}
		return nil, fmt.Errorf("stat index directory: %w", err)
	}
	if !fi.IsDir() {
		return nil, ErrIndexNotFound
	}

	return &Index{
		name:    name,
		baseDir: idxDir,
	}, nil
}

// ListIndices scans the data directory and returns all indices with their segment counts.
func (m *Manager) ListIndices() ([]IndexInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	indicesDir := filepath.Join(m.dataDir, "indices")
	entries, err := os.ReadDir(indicesDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []IndexInfo{}, nil
		}
		return nil, fmt.Errorf("read indices directory: %w", err)
	}

	result := make([]IndexInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if err := ValidateIndexName(name); err != nil {
			// Skip invalid or non-index directories
			continue
		}

		segCount := 0
		segFile := filepath.Join(indicesDir, name, "segments.json")
		if data, err := os.ReadFile(segFile); err == nil {
			var cp CommitPoint
			if err := json.Unmarshal(data, &cp); err == nil {
				segCount = len(cp.Segments)
			}
		}

		result = append(result, IndexInfo{
			Name:         name,
			SegmentCount: segCount,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result, nil
}
