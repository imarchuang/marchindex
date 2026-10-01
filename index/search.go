package index

import (
	"fmt"
	"time"
)

// SearchResult is the boolean search response.
// DocsExamined counts stored documents fetched after the postings combine.
// PostingsLookups counts term postings-list lookups, one per query term.
type SearchResult struct {
	Hits            []map[string]string `json:"hits"`
	TookMs          int64               `json:"took_ms"`
	PostingsLookups int                 `json:"postings_lookups"`
	DocsExamined    int                 `json:"docs_examined"`
}

// Search parses q and runs it against the in-memory index.
// limit caps how many stored docs are fetched, in increasing docID order.
func (idx *Index) Search(q string, limit int) (SearchResult, error) {
	if idx == nil || idx.ram == nil {
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
	res := idx.ram.search(node, limit)
	res.TookMs = time.Since(start).Milliseconds()
	return res, nil
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
