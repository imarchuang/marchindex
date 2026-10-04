package index

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// forceMergeAt is the minimum committed segments a forced merge will compact.
	forceMergeAt = 2
	// autoMergeAt is the size at which a flush compacts every committed segment.
	autoMergeAt = 3
)

// MergeResult describes one compaction of the commit point.
type MergeResult struct {
	Merged         bool   `json:"merged"`
	Segment        string `json:"segment"`
	SegmentsBefore int    `json:"segments_before"`
	SegmentsAfter  int    `json:"segments_after"`
	Docs           int    `json:"docs"`
	Dropped        int    `json:"dropped"`
	Generation     uint64 `json:"generation"`
}

// ForceMerge rewrites every committed segment into one segment.
// One or zero segments is a no-op. The RAM buffer is left alone.
func (idx *Index) ForceMerge() (MergeResult, error) {
	if idx == nil || idx.live == nil || idx.ram == nil {
		return MergeResult{}, fmt.Errorf("index is not open")
	}
	idx.live.mu.Lock()
	defer idx.live.mu.Unlock()
	return idx.mergeLocked(forceMergeAt)
}

// mergeLocked replaces the commit point only after the new segment directory
// is fully written. Readers keep seeing the input segments until that swap.
// Input directories are removed after the swap, so a crash never publishes
// a half-written merge or hides the inputs first.
func (idx *Index) mergeLocked(minSegments int) (MergeResult, error) {
	cp, err := readCommit(idx.SegmentsFilePath())
	if err != nil {
		return MergeResult{}, err
	}
	before := len(cp.Segments)
	if before < minSegments {
		return MergeResult{
			SegmentsBefore: before,
			SegmentsAfter:  before,
			Generation:     cp.Generation,
		}, nil
	}

	segs, err := readCommittedSegments(idx.baseDir)
	if err != nil {
		return MergeResult{}, err
	}
	docs, postings, dropped, err := mergeSegments(segs)
	if err != nil {
		return MergeResult{}, err
	}

	nextGen := cp.Generation + 1
	var refs []SegmentRef
	var newID string
	if len(docs) > 0 {
		newID, err = nextSegmentID(idx.SegmentsDir(), cp)
		if err != nil {
			return MergeResult{}, err
		}
		finalDir := filepath.Join(idx.SegmentsDir(), newID)
		partial := finalDir + ".partial"
		if err := os.RemoveAll(partial); err != nil {
			return MergeResult{}, fmt.Errorf("clear partial merge: %w", err)
		}
		if err := writeSegment(partial, newID, docs, postings, time.Now()); err != nil {
			_ = os.RemoveAll(partial)
			return MergeResult{}, err
		}
		if err := os.Rename(partial, finalDir); err != nil {
			_ = os.RemoveAll(partial)
			return MergeResult{}, fmt.Errorf("publish merged segment %s: %w", newID, err)
		}
		if err := syncDir(idx.SegmentsDir()); err != nil {
			return MergeResult{}, fmt.Errorf("sync segments directory: %w", err)
		}
		refs = []SegmentRef{{ID: newID, Generation: nextGen}}
	}

	next := CommitPoint{Generation: nextGen, Segments: refs}
	if err := writeCommit(idx.SegmentsFilePath(), next); err != nil {
		if newID != "" {
			_ = os.RemoveAll(filepath.Join(idx.SegmentsDir(), newID))
		}
		return MergeResult{}, err
	}
	for _, ref := range cp.Segments {
		if err := os.RemoveAll(filepath.Join(idx.SegmentsDir(), ref.ID)); err != nil {
			return MergeResult{}, fmt.Errorf("remove merged input %s: %w", ref.ID, err)
		}
	}

	after := len(refs)
	return MergeResult{
		Merged:         true,
		Segment:        newID,
		SegmentsBefore: before,
		SegmentsAfter:  after,
		Docs:           len(docs),
		Dropped:        dropped,
		Generation:     nextGen,
	}, nil
}

// mergeSegments assigns fresh local docIDs in commit order, skipping deletes.
// Postings from later segments receive higher docIDs, so appending them keeps
// each list strictly increasing.
func mergeSegments(segs []*segment) (docs []map[string]string, postings map[string][]docPosting, dropped int, err error) {
	postings = make(map[string][]docPosting)
	for _, seg := range segs {
		remap := make(map[uint32]uint32, len(seg.docOff))
		for old := range seg.docOff {
			docID := uint32(old)
			if seg.isDeleted(docID) {
				dropped++
				continue
			}
			fields, err := seg.fetch(docID)
			if err != nil {
				return nil, nil, 0, err
			}
			remap[docID] = uint32(len(docs))
			docs = append(docs, fields)
		}
		for term, list := range seg.postings {
			for _, old := range list {
				newID, ok := remap[old.doc]
				if !ok {
					continue
				}
				postings[term] = append(postings[term], clonePosting(old, newID))
			}
		}
	}
	if docs == nil {
		docs = []map[string]string{}
	}
	return docs, postings, dropped, nil
}
