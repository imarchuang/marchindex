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
	// ErrDocNotFound is returned when delete finds no live copy of an _id.
	ErrDocNotFound = errors.New("document not found")

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

// SegmentRef is one immutable segment listed in the commit point.
// Readers ignore segment directories that are not in this list.
type SegmentRef struct {
	ID         string `json:"id"`
	Generation uint64 `json:"generation"`
}

// CommitPoint is segments.json. Readers see a segment only after this file
// has been atomically replaced to include it.
type CommitPoint struct {
	Generation uint64       `json:"generation"`
	Segments   []SegmentRef `json:"segments"`
}

// indexLive is the process-local state shared by every handle for one index.
// mu serializes flush (writer) with search (reader) so a search never observes
// a new commit point together with the RAM docs that were just frozen into it.
type indexLive struct {
	mu  sync.RWMutex
	ram *RAMIndex
}

// IndexInfo provides basic metadata for listing indices.
type IndexInfo struct {
	Name         string `json:"name"`
	SegmentCount int    `json:"segment_count"`
}

// Index is a named index: its on-disk directory plus the shared RAM buffer.
// Search reads committed segments and, for near-real-time hits, the RAM buffer.
type Index struct {
	name    string
	baseDir string
	live    *indexLive
	ram     *RAMIndex
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
	live    map[string]*indexLive
}

// NewManager creates a new Manager instance pointing to dataDir.
func NewManager(dataDir string) (*Manager, error) {
	indicesDir := filepath.Join(dataDir, "indices")
	if err := os.MkdirAll(indicesDir, 0755); err != nil {
		return nil, fmt.Errorf("create indices root dir: %w", err)
	}
	return &Manager{
		dataDir: dataDir,
		live:    make(map[string]*indexLive),
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

	cp := CommitPoint{Segments: []SegmentRef{}}
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

	live := &indexLive{ram: newRAMIndex()}
	m.live[name] = live
	return &Index{
		name:    name,
		baseDir: idxDir,
		live:    live,
		ram:     live.ram,
	}, nil
}

// GetIndex retrieves an existing index by name.
// The returned Index shares the process-local RAM buffer for that name.
// A new process starts with an empty buffer and serves search from segments.json.
func (m *Manager) GetIndex(name string) (*Index, error) {
	if err := ValidateIndexName(name); err != nil {
		return nil, err
	}

	m.mu.RLock()
	idxDir := filepath.Join(m.dataDir, "indices", name)
	fi, err := os.Stat(idxDir)
	live := m.live[name]
	m.mu.RUnlock()

	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrIndexNotFound
		}
		return nil, fmt.Errorf("stat index directory: %w", err)
	}
	if !fi.IsDir() {
		return nil, ErrIndexNotFound
	}

	if live == nil {
		// Disk index from a previous process has no unflushed buffer yet.
		m.mu.Lock()
		live = m.live[name]
		if live == nil {
			live = &indexLive{ram: newRAMIndex()}
			if m.live == nil {
				m.live = make(map[string]*indexLive)
			}
			m.live[name] = live
		}
		m.mu.Unlock()
	}

	return &Index{
		name:    name,
		baseDir: idxDir,
		live:    live,
		ram:     live.ram,
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
