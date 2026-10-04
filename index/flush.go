package index

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// FlushResult is the response for freezing the RAM buffer into a segment.
type FlushResult struct {
	Flushed    bool   `json:"flushed"`
	Segment    string `json:"segment"`
	Docs       int    `json:"docs"`
	Terms      int    `json:"terms"`
	Generation uint64 `json:"generation"`
}

// frozen is one RAM buffer captured for a segment write.
// Local docIDs in docs/postings are 0..n-1. origDocIDs[i] is the RAM docID
// that became local doc i, so a successful commit can drop those rows.
type frozen struct {
	docs       []map[string]string
	postings   map[string][]docPosting
	origDocIDs []uint32
	idAt       map[string]uint32
}

// Flush freezes the RAM buffer into a new immutable segment and commits it
// by replacing segments.json. An empty buffer does not create a segment.
//
// The segment directory is renamed into place before segments.json changes.
// A crash between those two steps leaves an unlisted directory, which search
// ignores. Documents indexed while the files are being written stay in RAM.
//
// Re-indexing an _id that already lives in a committed segment does not hide
// the old copy. Slice 3's delete bitset is what makes the old postings skip.
func (idx *Index) Flush() (FlushResult, error) {
	if idx == nil || idx.live == nil || idx.ram == nil {
		return FlushResult{}, fmt.Errorf("index is not open")
	}

	idx.live.mu.Lock()
	defer idx.live.mu.Unlock()

	snap := idx.ram.snapshotFrozen()
	cp, err := readCommit(idx.SegmentsFilePath())
	if err != nil {
		return FlushResult{}, err
	}
	if snap == nil {
		return FlushResult{Generation: cp.Generation}, nil
	}

	id, err := nextSegmentID(idx.SegmentsDir(), cp)
	if err != nil {
		return FlushResult{}, err
	}
	finalDir := filepath.Join(idx.SegmentsDir(), id)
	partial := finalDir + ".partial"
	if err := os.RemoveAll(partial); err != nil {
		return FlushResult{}, fmt.Errorf("clear partial segment: %w", err)
	}
	if err := writeSegment(partial, id, snap.docs, snap.postings, time.Now()); err != nil {
		_ = os.RemoveAll(partial)
		return FlushResult{}, err
	}
	if err := os.Rename(partial, finalDir); err != nil {
		_ = os.RemoveAll(partial)
		return FlushResult{}, fmt.Errorf("publish segment %s: %w", id, err)
	}
	if err := syncDir(idx.SegmentsDir()); err != nil {
		return FlushResult{}, fmt.Errorf("sync segments directory: %w", err)
	}

	cp.Generation++
	cp.Segments = append(cp.Segments, SegmentRef{ID: id, Generation: cp.Generation})
	if err := writeCommit(idx.SegmentsFilePath(), cp); err != nil {
		_ = os.RemoveAll(finalDir)
		return FlushResult{}, err
	}
	idx.ram.dropFrozen(snap)

	// Three committed segments is enough to show the background merge.
	// Two flushes stay unmerged so a caller can still see each segment.
	if _, err := idx.mergeLocked(autoMergeAt); err != nil {
		return FlushResult{}, err
	}

	return FlushResult{
		Flushed:    true,
		Segment:    id,
		Docs:       len(snap.docs),
		Terms:      len(snap.postings),
		Generation: cp.Generation,
	}, nil
}

func nextSegmentID(dir string, cp CommitPoint) (string, error) {
	max := 0
	consider := func(name string) {
		var n int
		if _, err := fmt.Sscanf(name, "seg-%d", &n); err == nil && n > max {
			max = n
		}
	}
	for _, ref := range cp.Segments {
		consider(ref.ID)
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("read segments directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if len(name) > len(".partial") && name[len(name)-len(".partial"):] == ".partial" {
			name = name[:len(name)-len(".partial")]
		}
		consider(name)
	}
	return fmt.Sprintf("seg-%06d", max+1), nil
}
