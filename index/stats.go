package index

import (
	"fmt"
	"os"
	"path/filepath"
)

// SegmentStats is one committed segment.
// Docs counts every stored document. Deleted is how many of those have a bit set.
// Terms is the number of posting lists. Bytes is the sum of the segment files.
type SegmentStats struct {
	ID      string `json:"id"`
	Docs    int    `json:"docs"`
	Deleted int    `json:"deleted"`
	Live    int    `json:"live"`
	Terms   int    `json:"terms"`
	Bytes   int64  `json:"bytes"`
}

// Stats is the index as search currently sees it: committed segments plus the RAM buffer.
type Stats struct {
	Name       string         `json:"name"`
	Generation uint64         `json:"generation"`
	Docs       int            `json:"docs"`
	Deleted    int            `json:"deleted"`
	RamDocs    int            `json:"ram_docs"`
	RamTerms   int            `json:"ram_terms"`
	Segments   []SegmentStats `json:"segments"`
}

// Stats reads the commit point, each segment's meta and deleted.bits, and the RAM buffer.
func (idx *Index) Stats() (Stats, error) {
	if idx == nil || idx.live == nil || idx.ram == nil {
		return Stats{}, fmt.Errorf("index is not open")
	}
	idx.live.mu.RLock()
	defer idx.live.mu.RUnlock()

	cp, err := readCommit(idx.SegmentsFilePath())
	if err != nil {
		return Stats{}, err
	}
	out := Stats{
		Name:       idx.name,
		Generation: cp.Generation,
		Segments:   []SegmentStats{},
		RamDocs:    idx.ram.liveCount(),
		RamTerms:   idx.ram.termCount(),
	}
	for _, ref := range cp.Segments {
		seg, err := openSegment(filepath.Join(idx.SegmentsDir(), ref.ID), ref.ID)
		if err != nil {
			return Stats{}, err
		}
		deleted := seg.deletedCount()
		bytes, err := dirBytes(filepath.Join(idx.SegmentsDir(), ref.ID))
		if err != nil {
			return Stats{}, err
		}
		st := SegmentStats{
			ID:      ref.ID,
			Docs:    len(seg.docOff),
			Deleted: deleted,
			Live:    len(seg.docOff) - deleted,
			Terms:   len(seg.postings),
			Bytes:   bytes,
		}
		out.Segments = append(out.Segments, st)
		out.Docs += st.Live
		out.Deleted += deleted
	}
	out.Docs += out.RamDocs
	return out, nil
}

func (s *segment) deletedCount() int {
	n := 0
	for i := range s.docOff {
		if s.isDeleted(uint32(i)) {
			n++
		}
	}
	return n
}

func (r *RAMIndex) termCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.postings)
}

func dirBytes(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("stat segment: %w", err)
	}
	var total int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}
