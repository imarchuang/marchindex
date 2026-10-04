package index

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SearchIO counts bytes returned by application file reads, not physical disk
// traffic (the OS may satisfy reads from its page cache). Headers count as
// metadata; postings/docs count only the requested payload ranges.
type SearchIO struct {
	MetadataBytesRead int64 `json:"metadata_bytes_read"`
	PostingsBytesRead int64 `json:"postings_bytes_read"`
	DocsBytesRead     int64 `json:"docs_bytes_read"`
	PostingsDecoded   int   `json:"postings_decoded"`
}

// segmentReader is request-scoped. Only the dictionary, offsets and deletion
// bits live in memory. Files are closed before search releases live.mu, so a
// merge cannot remove files from under a reader. No cross-query cache or FDs
// survive a request; deletion bits are therefore always fresh.
type segmentReader struct {
	segment  // only id, docOff and deleted; eager docs/postings are unused
	terms    map[string]termRef
	posts    *os.File
	store    *os.File
	docsSize uint64
	io       *SearchIO
}

func readMetadata(path string, stats *SearchIO) ([]byte, error) {
	b, err := os.ReadFile(path)
	stats.MetadataBytesRead += int64(len(b))
	return b, err
}

func readRange(f *os.File, off uint64, size uint64, counter *int64) ([]byte, error) {
	if size > uint64(^uint(0)>>1) || off > uint64(1<<63-1) {
		return nil, fmt.Errorf("%s: range too large", f.Name())
	}
	b := make([]byte, int(size))
	n, err := f.ReadAt(b, int64(off))
	*counter += int64(n)
	if err != nil {
		return nil, fmt.Errorf("%s at %d: %w", f.Name(), off, err)
	}
	return b, nil
}

func openSegmentReader(dir, id string, stats *SearchIO) (_ *segmentReader, err error) {
	r := &segmentReader{segment: segment{id: id}, io: stats}
	defer func() {
		if err != nil {
			r.close()
		}
	}()
	metaBytes, err := readMetadata(filepath.Join(dir, "meta.json"), stats)
	if err != nil {
		return nil, err
	}
	var meta segmentMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, err
	}
	if meta.ID != id || meta.Docs < 0 || meta.Terms < 0 {
		return nil, fmt.Errorf("segment %s: invalid metadata", id)
	}
	r.posts, err = os.Open(filepath.Join(dir, "postings.bin"))
	if err != nil {
		return nil, err
	}
	postInfo, err := r.posts.Stat()
	if err != nil {
		return nil, err
	}
	header, err := readRange(r.posts, 0, 8, &stats.MetadataBytesRead)
	if err != nil {
		return nil, err
	}
	if err := checkHeader(header, postingsMagic); err != nil {
		return nil, err
	}
	terms, err := readMetadata(filepath.Join(dir, "terms.bin"), stats)
	if err != nil {
		return nil, err
	}
	r.terms, err = decodeTermRefs(terms, uint64(postInfo.Size()))
	if err != nil {
		return nil, err
	}
	if len(r.terms) != meta.Terms {
		return nil, fmt.Errorf("segment %s: term count mismatch", id)
	}

	r.store, err = os.Open(filepath.Join(dir, "docs.bin"))
	if err != nil {
		return nil, err
	}
	docInfo, err := r.store.Stat()
	if err != nil {
		return nil, err
	}
	r.docsSize = uint64(docInfo.Size())
	header, err = readRange(r.store, 0, 12, &stats.MetadataBytesRead)
	if err != nil {
		return nil, err
	}
	if err := checkHeader(header, docsMagic); err != nil {
		return nil, err
	}
	if uint64(binary.LittleEndian.Uint32(header[8:])) != uint64(meta.Docs) {
		return nil, fmt.Errorf("segment %s: docs count mismatch", id)
	}
	offsets, err := readMetadata(filepath.Join(dir, "docs.idx"), stats)
	if err != nil {
		return nil, err
	}
	r.docOff, err = decodeDocsIndex(offsets)
	if err != nil {
		return nil, err
	}
	if len(r.docOff) != meta.Docs {
		return nil, fmt.Errorf("segment %s: doc index count mismatch", id)
	}
	for i, off := range r.docOff {
		if off < 12 || off >= r.docsSize || (i > 0 && off <= r.docOff[i-1]) {
			return nil, fmt.Errorf("segment %s: invalid offset for doc %d", id, i)
		}
	}
	bits, err := readMetadata(filepath.Join(dir, "deleted.bits"), stats)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		r.deleted, err = decodeDeletedBits(bits, meta.Docs)
		if err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *segmentReader) close() {
	if r.posts != nil {
		_ = r.posts.Close()
	}
	if r.store != nil {
		_ = r.store.Close()
	}
}

func readSearchSegments(baseDir string, stats *SearchIO) ([]*segmentReader, error) {
	data, err := readMetadata(filepath.Join(baseDir, "segments.json"), stats)
	if err != nil {
		return nil, err
	}
	var cp CommitPoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, err
	}
	var readers []*segmentReader
	for _, ref := range cp.Segments {
		r, err := openSegmentReader(filepath.Join(baseDir, "segments", ref.ID), ref.ID, stats)
		if err != nil {
			for _, opened := range readers {
				opened.close()
			}
			return nil, fmt.Errorf("segment %s: %w", ref.ID, err)
		}
		readers = append(readers, r)
	}
	return readers, nil
}

// queryPostings expands fuzzy leaves using dictionary keys only, then loads
// each distinct required list once. Evaluation and TF scoring share this map.
func (r *segmentReader) queryPostings(n *qNode) (map[string][]docPosting, error) {
	needed := make(map[string]struct{})
	var visit func(*qNode)
	visit = func(n *qNode) {
		if n == nil {
			return
		}
		switch n.kind {
		case qTerm:
			needed[scopedTerm(n.field, n.term)] = struct{}{}
		case qPhrase:
			for _, term := range n.phrase {
				needed[scopedTerm(n.field, term.term)] = struct{}{}
			}
		case qFuzzy:
			if n.fuzz == 0 {
				needed[scopedTerm(n.field, n.term)] = struct{}{}
			} else {
				prefix := scopedTerm(n.field, "")
				for key := range r.terms {
					term, ok := strings.CutPrefix(key, prefix)
					if ok && term != "" && withinEditDistance(n.term, term, n.fuzz) {
						needed[key] = struct{}{}
					}
				}
			}
		}
		for _, child := range n.kids {
			visit(child)
		}
	}
	visit(n)
	out := make(map[string][]docPosting, len(needed))
	for key := range needed {
		ref, ok := r.terms[key]
		if !ok {
			continue
		}
		blob, err := readRange(r.posts, ref.offset, uint64(ref.length), &r.io.PostingsBytesRead)
		if err != nil {
			return nil, err
		}
		list, err := decodePostings(blob)
		if err != nil {
			return nil, fmt.Errorf("segment %s term %q: %w", r.id, key, err)
		}
		if uint32(len(list)) != ref.df {
			return nil, fmt.Errorf("segment %s term %q: df mismatch", r.id, key)
		}
		for _, p := range list {
			if uint64(p.doc) >= uint64(len(r.docOff)) {
				return nil, fmt.Errorf("segment %s term %q: docID out of range", r.id, key)
			}
		}
		r.io.PostingsDecoded++
		out[key] = list
	}
	return out, nil
}

func (r *segmentReader) fetch(docID uint32) (map[string]string, error) {
	if uint64(docID) >= uint64(len(r.docOff)) {
		return nil, fmt.Errorf("segment %s: doc %d out of range", r.id, docID)
	}
	off := r.docOff[docID]
	end := r.docsSize
	if int(docID)+1 < len(r.docOff) {
		end = r.docOff[int(docID)+1]
	}
	blob, err := readRange(r.store, off, end-off, &r.io.DocsBytesRead)
	if err != nil {
		return nil, err
	}
	n, k := binary.Uvarint(blob)
	if k <= 0 || n != uint64(len(blob)-k) {
		return nil, fmt.Errorf("segment %s: doc %d invalid length", r.id, docID)
	}
	var fields map[string]string
	if err := json.Unmarshal(blob[k:], &fields); err != nil {
		return nil, fmt.Errorf("segment %s: doc %d: %w", r.id, docID, err)
	}
	if fields == nil {
		fields = map[string]string{}
	}
	return fields, nil
}
