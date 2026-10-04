package index

import (
	"fmt"
	"sort"
	"strconv"
	"sync"
)

// RAMIndex is one index's in-memory inverted index and doc store.
// docIDs are local to the index and start at 0. A later slice will freeze
// this buffer into an immutable segment; until then there is one RAM index
// per name and search does not read segment files.
type RAMIndex struct {
	mu sync.RWMutex

	// postings maps "field:term" to a strictly increasing docID list.
	postings map[string][]uint32
	// docs is the doc store: docID → original field map, including _id.
	docs map[uint32]map[string]string
	// docTerms records the terms written for a docID.
	// Re-indexing an _id removes those postings and stores the new body
	// under a fresh docID. The old docID is not reused. Slice 3 will track
	// deletes with a bitset; slice 1 drops the old postings instead so a
	// replaced document no longer matches its previous terms.
	docTerms map[uint32][]string
	idToDoc  map[string]uint32

	nextDoc  uint32
	nextAuto uint64
}

// IndexResult is returned by IndexDocument.
type IndexResult struct {
	ID       string `json:"_id"`
	Seq      uint32 `json:"_seq"`
	Replaced bool   `json:"-"`
}

func newRAMIndex() *RAMIndex {
	return &RAMIndex{
		postings: make(map[string][]uint32),
		docs:     make(map[uint32]map[string]string),
		docTerms: make(map[uint32][]string),
		idToDoc:  make(map[string]uint32),
	}
}

// IndexDocument analyzes fields and adds them to the RAM index.
// An empty id assigns the next free monotonic id ("1", "2", ...).
// Indexing an _id that already exists replaces that document.
func (idx *Index) IndexDocument(id string, fields map[string]string) (IndexResult, error) {
	if idx == nil || idx.ram == nil {
		return IndexResult{}, fmt.Errorf("index is not open")
	}
	return idx.ram.add(id, fields)
}

func (r *RAMIndex) add(id string, fields map[string]string) (IndexResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	id, err := r.assignIDLocked(id)
	if err != nil {
		return IndexResult{}, err
	}

	replaced := false
	if old, ok := r.idToDoc[id]; ok {
		r.dropLocked(old)
		delete(r.idToDoc, id)
		replaced = true
	}

	docID := r.nextDoc
	r.nextDoc++

	terms := FieldTerms(fields)
	for _, term := range terms {
		r.postings[term] = append(r.postings[term], docID)
	}
	if terms == nil {
		terms = []string{}
	}
	r.docTerms[docID] = terms

	stored := make(map[string]string, len(fields)+1)
	for k, v := range fields {
		if k == "_id" {
			continue
		}
		stored[k] = v
	}
	stored["_id"] = id
	r.docs[docID] = stored
	r.idToDoc[id] = docID

	return IndexResult{ID: id, Seq: docID, Replaced: replaced}, nil
}

func (r *RAMIndex) assignIDLocked(id string) (string, error) {
	if id != "" {
		return id, nil
	}
	for {
		r.nextAuto++
		if r.nextAuto == 0 {
			return "", fmt.Errorf("document id space exhausted")
		}
		candidate := strconv.FormatUint(r.nextAuto, 10)
		if _, exists := r.idToDoc[candidate]; !exists {
			return candidate, nil
		}
	}
}

// deleteID drops the live RAM copy of id. A committed copy is untouched.
func (r *RAMIndex) deleteID(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	docID, ok := r.idToDoc[id]
	if !ok {
		return false
	}
	r.dropLocked(docID)
	delete(r.idToDoc, id)
	if len(r.docs) == 0 {
		r.nextDoc = 0
		r.postings = make(map[string][]uint32)
		r.docs = make(map[uint32]map[string]string)
		r.docTerms = make(map[uint32][]string)
	}
	return true
}

func (r *RAMIndex) dropLocked(docID uint32) {
	for _, term := range r.docTerms[docID] {
		list := removeDocID(r.postings[term], docID)
		if len(list) == 0 {
			delete(r.postings, term)
		} else {
			r.postings[term] = list
		}
	}
	delete(r.docTerms, docID)
	delete(r.docs, docID)
}

func removeDocID(ids []uint32, id uint32) []uint32 {
	i := sort.Search(len(ids), func(j int) bool { return ids[j] >= id })
	if i == len(ids) || ids[i] != id {
		return ids
	}
	copy(ids[i:], ids[i+1:])
	return ids[:len(ids)-1]
}

// snapshotFrozen copies the live buffer and remaps docIDs to 0..n-1.
// The caller writes that copy, then dropFrozen removes the original rows.
func (r *RAMIndex) snapshotFrozen() *frozen {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.docs) == 0 {
		return nil
	}

	orig := make([]uint32, 0, len(r.docs))
	for id := range r.docs {
		orig = append(orig, id)
	}
	sort.Slice(orig, func(i, j int) bool { return orig[i] < orig[j] })

	remap := make(map[uint32]uint32, len(orig))
	docs := make([]map[string]string, len(orig))
	idAt := make(map[string]uint32, len(orig))
	for newID, old := range orig {
		remap[old] = uint32(newID)
		docs[newID] = cloneMap(r.docs[old])
		idAt[r.docs[old]["_id"]] = old
	}

	postings := make(map[string][]uint32, len(r.postings))
	for term, list := range r.postings {
		out := make([]uint32, 0, len(list))
		for _, old := range list {
			if nid, ok := remap[old]; ok {
				out = append(out, nid)
			}
		}
		if len(out) > 0 {
			postings[term] = out
		}
	}
	return &frozen{
		docs:       docs,
		postings:   postings,
		origDocIDs: orig,
		idAt:       idAt,
	}
}

// dropFrozen removes rows that were committed, if they are still the current
// version of that _id. A replacement indexed during the file write stays.
func (r *RAMIndex) dropFrozen(f *frozen) {
	if f == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, old := range f.origDocIDs {
		if _, ok := r.docs[old]; !ok {
			continue
		}
		r.dropLocked(old)
	}
	for id, old := range f.idAt {
		if r.idToDoc[id] == old {
			delete(r.idToDoc, id)
		}
	}
	if len(r.docs) == 0 {
		r.nextDoc = 0
		r.postings = make(map[string][]uint32)
		r.docs = make(map[uint32]map[string]string)
		r.docTerms = make(map[uint32][]string)
	}
}

// match evaluates n against the RAM buffer. Lookups count even when limit
// fetches nothing. An empty buffer reports zero lookups so a flushed index
// is not charged for an idle buffer.
func (r *RAMIndex) match(n *qNode, limit int) (hits []map[string]string, docIDs []uint32, lookups int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.docs) == 0 {
		return nil, nil, 0
	}
	ids := n.eval(r.postings, &lookups)
	if limit > len(ids) {
		limit = len(ids)
	}
	if limit < 0 {
		limit = 0
	}
	hits = make([]map[string]string, 0, limit)
	docIDs = make([]uint32, 0, limit)
	for _, id := range ids[:limit] {
		hits = append(hits, cloneMap(r.docs[id]))
		docIDs = append(docIDs, id)
	}
	return hits, docIDs, lookups
}

func (idx *Index) storedDocs() []map[string]string {
	if idx == nil || idx.ram == nil {
		return nil
	}
	return idx.ram.snapshot()
}

func (r *RAMIndex) snapshot() []map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]uint32, 0, len(r.docs))
	for id := range r.docs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, cloneMap(r.docs[id]))
	}
	return out
}

func (idx *Index) checkPostings() error {
	if idx == nil || idx.ram == nil {
		return fmt.Errorf("index is not open")
	}
	r := idx.ram
	r.mu.RLock()
	defer r.mu.RUnlock()
	for term, list := range r.postings {
		for i := 1; i < len(list); i++ {
			if list[i] <= list[i-1] {
				return fmt.Errorf("postings %q are not strictly increasing", term)
			}
		}
		for _, id := range list {
			if _, ok := r.docs[id]; !ok {
				return fmt.Errorf("postings %q reference missing doc %d", term, id)
			}
		}
	}
	return nil
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
