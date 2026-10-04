package index

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"
)

// SearchResult is the boolean search response.
// DocsExamined counts stored documents fetched after the postings combine.
// PostingsLookups counts term postings-list lookups. Each committed segment
// and a non-empty RAM buffer contributes one lookup per exact query term.
// A fuzzy term with distance 0 does the same. A wider fuzzy term contributes
// one lookup per dictionary term within that distance; zero matches add none.
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
	return idx.search(q, limit, false)
}

// SearchTF is Search, then orders matches by the sum of matching term
// frequencies. Ties keep commit order. The response field _score is that sum.
func (idx *Index) SearchTF(q string, limit int) (SearchResult, error) {
	return idx.search(q, limit, true)
}

func (idx *Index) search(q string, limit int, rankTF bool) (SearchResult, error) {
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
	type candidate struct {
		fetch func() (map[string]string, error)
		score int
	}
	var cands []candidate
	for _, seg := range segs {
		ids := seg.liveIDs(node.eval(seg.postings, &lookups))
		for _, id := range ids {
			id := id
			seg := seg
			score := 0
			if rankTF {
				score = node.score(seg.postings, id)
			}
			cands = append(cands, candidate{
				score: score,
				fetch: func() (map[string]string, error) {
					doc, err := seg.fetch(id)
					if err != nil {
						return nil, err
					}
					doc["_seg"] = seg.id
					doc["_doc"] = strconv.FormatUint(uint64(id), 10)
					if rankTF {
						doc["_score"] = strconv.Itoa(score)
					}
					return doc, nil
				},
			})
		}
	}

	ramLimit := limit
	if rankTF {
		ramLimit = math.MaxInt
	}
	ramHits, ramDocs, ramScores, ramLookups := idx.ram.match(node, ramLimit)
	lookups += ramLookups
	for i, doc := range ramHits {
		i := i
		doc := doc
		score := 0
		if rankTF {
			score = ramScores[i]
		}
		cands = append(cands, candidate{
			score: score,
			fetch: func() (map[string]string, error) {
				doc["_seg"] = "_ram"
				doc["_doc"] = strconv.FormatUint(uint64(ramDocs[i]), 10)
				if rankTF {
					doc["_score"] = strconv.Itoa(score)
				}
				return doc, nil
			},
		})
	}

	if rankTF {
		sort.SliceStable(cands, func(i, j int) bool {
			return cands[i].score > cands[j].score
		})
	} else if limit < len(cands) {
		cands = cands[:limit]
	}
	if rankTF && limit < len(cands) {
		cands = cands[:limit]
	}

	hits := make([]map[string]string, 0, len(cands))
	for _, cand := range cands {
		doc, err := cand.fetch()
		if err != nil {
			return SearchResult{}, err
		}
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
func (n *qNode) eval(postings map[string][]docPosting, lookups *int) []uint32 {
	if n == nil {
		return nil
	}
	switch n.kind {
	case qTerm:
		*lookups++
		return append([]uint32(nil), postingIDs(postings[scopedTerm(n.field, n.term)])...)
	case qFuzzy:
		return fuzzyDocs(postings, n.field, n.term, n.fuzz, lookups)
	case qPhrase:
		return phraseDocs(postings, n.field, n.phrase, lookups)
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

// phraseDocs returns documents where each phrase term occurs at
// firstPosition+delta. A delta of 1 means the next term is the next token.
func phraseDocs(postings map[string][]docPosting, field string, phrase []phraseTerm, lookups *int) []uint32 {
	if len(phrase) == 0 {
		return nil
	}
	lists := make([][]docPosting, len(phrase))
	for i, term := range phrase {
		*lookups++
		lists[i] = postings[scopedTerm(field, term.term)]
	}
	for _, list := range lists {
		if len(list) == 0 {
			return nil
		}
	}
	var out []uint32
	idx := make([]int, len(lists))
	for idx[0] < len(lists[0]) {
		doc := lists[0][idx[0]].doc
		aligned := true
		for i := 1; i < len(lists); i++ {
			for idx[i] < len(lists[i]) && lists[i][idx[i]].doc < doc {
				idx[i]++
			}
			if idx[i] >= len(lists[i]) || lists[i][idx[i]].doc != doc {
				aligned = false
				break
			}
		}
		if aligned && phraseAligned(lists, idx, phrase) {
			out = append(out, doc)
		}
		idx[0]++
	}
	return out
}

func phraseAligned(lists [][]docPosting, idx []int, phrase []phraseTerm) bool {
	first := lists[0][idx[0]].pos
	for _, start := range first {
		ok := true
		for i := 1; i < len(phrase); i++ {
			if !hasPos(lists[i][idx[i]].pos, start+phrase[i].delta) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func hasPos(pos []uint32, want uint32) bool {
	i := sort.Search(len(pos), func(j int) bool { return pos[j] >= want })
	return i < len(pos) && pos[i] == want
}

// score is the sum of term frequencies for the terms that contribute to a
// match. A phrase contributes how many times it occurs in the document.
func (n *qNode) score(postings map[string][]docPosting, doc uint32) int {
	if n == nil {
		return 0
	}
	switch n.kind {
	case qTerm:
		p, ok := findPosting(postings[scopedTerm(n.field, n.term)], doc)
		if !ok {
			return 0
		}
		return len(p.pos)
	case qFuzzy:
		if n.fuzz == 0 {
			p, ok := findPosting(postings[scopedTerm(n.field, n.term)], doc)
			if !ok {
				return 0
			}
			return len(p.pos)
		}
		total := 0
		for _, key := range fuzzyTermKeys(postings, n.field, n.term, n.fuzz) {
			p, ok := findPosting(postings[key], doc)
			if ok {
				total += len(p.pos)
			}
		}
		return total
	case qPhrase:
		return phraseScore(postings, n.field, n.phrase, doc)
	case qAnd, qOr:
		total := 0
		for _, k := range n.kids {
			total += k.score(postings, doc)
		}
		return total
	default:
		return 0
	}
}

func findPosting(list []docPosting, doc uint32) (docPosting, bool) {
	i := sort.Search(len(list), func(j int) bool { return list[j].doc >= doc })
	if i < len(list) && list[i].doc == doc {
		return list[i], true
	}
	return docPosting{}, false
}

func phraseScore(postings map[string][]docPosting, field string, phrase []phraseTerm, doc uint32) int {
	if len(phrase) == 0 {
		return 0
	}
	pos := make([][]uint32, len(phrase))
	for i, term := range phrase {
		p, ok := findPosting(postings[scopedTerm(field, term.term)], doc)
		if !ok {
			return 0
		}
		pos[i] = p.pos
	}
	n := 0
	for _, start := range pos[0] {
		ok := true
		for i := 1; i < len(phrase); i++ {
			if !hasPos(pos[i], start+phrase[i].delta) {
				ok = false
				break
			}
		}
		if ok {
			n++
		}
	}
	return n
}
