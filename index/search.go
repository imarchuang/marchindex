package index

import (
	"fmt"
	"strconv"
	"time"
)

// SearchResult is the boolean search response.
// DocsExamined counts stored documents fetched after the postings combine.
// PostingsLookups counts term postings-list lookups. Each committed segment
// and a non-empty RAM buffer contributes one lookup per query term.
// SegmentsSearched is the number of segments listed in the commit point.
type SearchResult struct {
	Hits             []map[string]string `json:"hits"`
	TookMs           int64               `json:"took_ms"`
	PostingsLookups  int                 `json:"postings_lookups"`
	DocsExamined     int                 `json:"docs_examined"`
	SegmentsSearched int                 `json:"segments_searched"`
}

// Search parses q and runs it against every committed segment, then the
// unflushed RAM buffer. limit caps how many stored docs are fetched.
// Segment hits come first, in commit order and increasing local docID;
// RAM hits follow in increasing docID.
func (idx *Index) Search(q string, limit int) (SearchResult, error) {
	if idx == nil || idx.live == nil || idx.ram == nil {
		return SearchResult{}, fmt.Errorf("index is not open")
	}
	if limit < 0 {
		return SearchResult{}, badQueryf("limit must be >= 0")
	}
	start := time.Now()
	node, err := parseQuery(q)
	if err != nil {
		return SearchResult{}, err
	}

	idx.live.mu.RLock()
	defer idx.live.mu.RUnlock()

	segs, err := readCommittedSegments(idx.baseDir)
	if err != nil {
		return SearchResult{}, err
	}

	var lookups int
	hits := make([]map[string]string, 0)
	remaining := limit
	for _, seg := range segs {
		ids := seg.liveIDs(node.eval(seg.postings, &lookups))
		for _, id := range ids {
			if remaining == 0 {
				break
			}
			doc, err := seg.fetch(id)
			if err != nil {
				return SearchResult{}, err
			}
			doc["_seg"] = seg.id
			doc["_doc"] = strconv.FormatUint(uint64(id), 10)
			hits = append(hits, doc)
			remaining--
		}
	}

	ramHits, ramDocs, ramLookups := idx.ram.match(node, remaining)
	lookups += ramLookups
	for i, doc := range ramHits {
		doc["_seg"] = "_ram"
		doc["_doc"] = strconv.FormatUint(uint64(ramDocs[i]), 10)
		hits = append(hits, doc)
	}

	return SearchResult{
		Hits:             hits,
		TookMs:           time.Since(start).Milliseconds(),
		PostingsLookups:  lookups,
		DocsExamined:     len(hits),
		SegmentsSearched: len(segs),
	}, nil
}

// eval combines postings for n. Each term list is copied so the result
// does not alias the live index.
func (n *qNode) eval(postings map[string][]uint32, lookups *int) []uint32 {
	if n == nil {
		return nil
	}
	switch n.kind {
	case qTerm:
		*lookups++
		src := postings[scopedTerm(n.field, n.term)]
		out := make([]uint32, len(src))
		copy(out, src)
		return out
	case qAnd:
		if len(n.kids) == 0 {
			return nil
		}
		acc := n.kids[0].eval(postings, lookups)
		for _, k := range n.kids[1:] {
			acc = intersect(acc, k.eval(postings, lookups))
		}
		return acc
	case qOr:
		if len(n.kids) == 0 {
			return nil
		}
		acc := n.kids[0].eval(postings, lookups)
		for _, k := range n.kids[1:] {
			acc = union(acc, k.eval(postings, lookups))
		}
		return acc
	default:
		return nil
	}
}

func intersect(a, b []uint32) []uint32 {
	out := make([]uint32, 0)
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return out
}

func union(a, b []uint32) []uint32 {
	out := make([]uint32, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			out = append(out, a[i])
			i++
		default:
			out = append(out, b[j])
			j++
		}
	}
	out = append(out, a[i:]...)
	out = append(out, b[j:]...)
	return out
}
