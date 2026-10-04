package index

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// On-disk segment layout (slice 2). A segment directory is invisible to
// search until segments.json lists its id.
//
//	meta.json     doc count, term count, created_ns
//	terms.bin     sorted term dictionary pointing into postings.bin
//	postings.bin  one delta-encoded docID list per term, with positions
//	docs.bin      stored field JSON, one record per local docID
//	docs.idx      docID → byte offset of that record in docs.bin
//
// terms.bin
//
//	magic "MXTR" | version uint32 | count uint32
//	repeated, in sorted term order:
//	  nameLen uint32 | name bytes | postOff uint64 | postLen uint32 | df uint32
//
// postings.bin
//
//	magic "MXPO" | version uint32
//	then blobs addressed by terms.bin. Each blob is:
//	  count uvarint
//	  repeated per document:
//	    docDelta uvarint | freq uvarint | pos0 uvarint | posDelta uvarint ...
//	docDelta is the docID for the first document and a strictly positive
//	difference after that. freq is the number of positions. pos0 is the first
//	position in the field; later positions are strictly positive differences.
//	A position counts every token in the field, including tokens that are too
//	short to index, so a dropped token still leaves a gap.
//
// docs.bin
//
//	magic "MXDC" | version uint32 | count uint32
//	repeated: jsonLen uvarint | json object bytes
//
// docs.idx
//
//	magic "MXDI" | version uint32 | count uint32
//	repeated pairs, docID ascending: docID uint32 | offset uint64
//	offset is the start of that doc's uvarint length in docs.bin.
//	Slice 2 writes a dense 0..n-1 id space; the pair list is the sparse form
//	so later deletes can omit ids without changing the file shape.

const (
	termsMagic    = "MXTR"
	postingsMagic = "MXPO"
	docsMagic     = "MXDC"
	docsIdxMagic  = "MXDI"
	// Version 2 stores a position list after each docID. Version 1 segments
	// have docIDs only and cannot answer phrase queries.
	formatVersion = 2
)

type segmentMeta struct {
	ID        string `json:"id"`
	Docs      int    `json:"docs"`
	Terms     int    `json:"terms"`
	CreatedNs int64  `json:"created_ns"`
}

// segment is the eager representation used by maintenance (merge/delete).
// It loads all postings and stored document bytes. Search uses segmentReader
// instead, reading only selected postings and document ranges.
type segment struct {
	id       string
	postings map[string][]docPosting
	docs     []byte
	docOff   []uint64
	// deleted is nil when the segment has no deleted.bits file.
	// A set bit means that local docID is skipped until merge rewrites the segment.
	deleted []byte
}

func (s *segment) fetch(docID uint32) (map[string]string, error) {
	if int(docID) >= len(s.docOff) {
		return nil, fmt.Errorf("segment %s: doc %d out of range", s.id, docID)
	}
	off := s.docOff[docID]
	if off > uint64(len(s.docs)) {
		return nil, fmt.Errorf("segment %s: doc %d offset past end", s.id, docID)
	}
	n, nlen := binary.Uvarint(s.docs[off:])
	if nlen <= 0 {
		return nil, fmt.Errorf("segment %s: doc %d length", s.id, docID)
	}
	start := off + uint64(nlen)
	end := start + n
	if end > uint64(len(s.docs)) {
		return nil, fmt.Errorf("segment %s: doc %d truncated", s.id, docID)
	}
	var fields map[string]string
	if err := json.Unmarshal(s.docs[start:end], &fields); err != nil {
		return nil, fmt.Errorf("segment %s: doc %d json: %w", s.id, docID, err)
	}
	if fields == nil {
		fields = map[string]string{}
	}
	return fields, nil
}

func readCommit(path string) (CommitPoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CommitPoint{}, fmt.Errorf("read segments.json: %w", err)
	}
	var cp CommitPoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return CommitPoint{}, fmt.Errorf("parse segments.json: %w", err)
	}
	if cp.Segments == nil {
		cp.Segments = []SegmentRef{}
	}
	return cp, nil
}

func writeCommit(path string, cp CommitPoint) error {
	if cp.Segments == nil {
		cp.Segments = []SegmentRef{}
	}
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal segments.json: %w", err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := writeFileSync(tmp, data); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace segments.json: %w", err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync index directory: %w", err)
	}
	return nil
}

func readCommittedSegments(baseDir string) ([]*segment, error) {
	cp, err := readCommit(filepath.Join(baseDir, "segments.json"))
	if err != nil {
		return nil, err
	}
	out := make([]*segment, 0, len(cp.Segments))
	for _, ref := range cp.Segments {
		seg, err := openSegment(filepath.Join(baseDir, "segments", ref.ID), ref.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, seg)
	}
	return out, nil
}

func openSegment(dir, id string) (*segment, error) {
	metaBytes, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, fmt.Errorf("segment %s: meta: %w", id, err)
	}
	var meta segmentMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, fmt.Errorf("segment %s: meta: %w", id, err)
	}
	if meta.ID != id {
		return nil, fmt.Errorf("segment %s: meta id %q", id, meta.ID)
	}

	postingsFile, err := os.ReadFile(filepath.Join(dir, "postings.bin"))
	if err != nil {
		return nil, fmt.Errorf("segment %s: postings: %w", id, err)
	}
	if err := checkHeader(postingsFile, postingsMagic); err != nil {
		return nil, fmt.Errorf("segment %s: postings: %w", id, err)
	}

	termsFile, err := os.ReadFile(filepath.Join(dir, "terms.bin"))
	if err != nil {
		return nil, fmt.Errorf("segment %s: terms: %w", id, err)
	}
	postings, err := decodeTermDictionary(termsFile, postingsFile)
	if err != nil {
		return nil, fmt.Errorf("segment %s: %w", id, err)
	}
	if len(postings) != meta.Terms {
		return nil, fmt.Errorf("segment %s: term count %d, meta %d", id, len(postings), meta.Terms)
	}

	docsFile, err := os.ReadFile(filepath.Join(dir, "docs.bin"))
	if err != nil {
		return nil, fmt.Errorf("segment %s: docs: %w", id, err)
	}
	if err := checkHeader(docsFile, docsMagic); err != nil {
		return nil, fmt.Errorf("segment %s: docs: %w", id, err)
	}
	if len(docsFile) < 12 {
		return nil, fmt.Errorf("segment %s: docs header truncated", id)
	}
	idxFile, err := os.ReadFile(filepath.Join(dir, "docs.idx"))
	if err != nil {
		return nil, fmt.Errorf("segment %s: docs.idx: %w", id, err)
	}
	docOff, err := decodeDocsIndex(idxFile)
	if err != nil {
		return nil, fmt.Errorf("segment %s: %w", id, err)
	}
	if len(docOff) != meta.Docs {
		return nil, fmt.Errorf("segment %s: doc count %d, meta %d", id, len(docOff), meta.Docs)
	}
	docsCount := binary.LittleEndian.Uint32(docsFile[8:12])
	if int(docsCount) != meta.Docs {
		return nil, fmt.Errorf("segment %s: docs.bin count %d, meta %d", id, docsCount, meta.Docs)
	}
	deleted, err := readDeletedBits(filepath.Join(dir, "deleted.bits"), meta.Docs)
	if err != nil {
		return nil, fmt.Errorf("segment %s: %w", id, err)
	}

	return &segment{
		id:       id,
		postings: postings,
		docs:     docsFile,
		docOff:   docOff,
		deleted:  deleted,
	}, nil
}

func (s *segment) isDeleted(docID uint32) bool {
	if s == nil || int(docID) >= len(s.docOff) {
		return false
	}
	byteIndex := int(docID / 8)
	if byteIndex >= len(s.deleted) {
		return false
	}
	return s.deleted[byteIndex]&(1<<uint(docID%8)) != 0
}

func (s *segment) liveIDs(ids []uint32) []uint32 {
	if len(s.deleted) == 0 {
		return ids
	}
	out := make([]uint32, 0, len(ids))
	for _, id := range ids {
		if !s.isDeleted(id) {
			out = append(out, id)
		}
	}
	return out
}

func writeSegment(dir, id string, docs []map[string]string, postings map[string][]docPosting, created time.Time) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create segment dir: %w", err)
	}
	terms, posts, err := encodeDictionary(postings)
	if err != nil {
		return err
	}
	docsBin, docsIdx, err := encodeDocs(docs)
	if err != nil {
		return err
	}
	meta := segmentMeta{
		ID:        id,
		Docs:      len(docs),
		Terms:     len(postings),
		CreatedNs: created.UnixNano(),
	}
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
	metaBytes = append(metaBytes, '\n')

	files := []struct {
		name string
		data []byte
	}{
		{"meta.json", metaBytes},
		{"terms.bin", terms},
		{"postings.bin", posts},
		{"docs.bin", docsBin},
		{"docs.idx", docsIdx},
	}
	for _, f := range files {
		if err := writeFileSync(filepath.Join(dir, f.name), f.data); err != nil {
			return err
		}
	}
	return syncDir(dir)
}

func encodeDictionary(postings map[string][]docPosting) (terms, posts []byte, err error) {
	names := make([]string, 0, len(postings))
	for term := range postings {
		names = append(names, term)
	}
	// Sort so the dictionary is inspectable and stable across flushes.
	sort.Strings(names)

	posts = append(posts, postingsMagic...)
	posts = appendU32(posts, formatVersion)

	type rec struct {
		name    string
		off     uint64
		length  uint32
		docFreq uint32
	}
	recs := make([]rec, 0, len(names))
	for _, name := range names {
		blob, err := encodePostings(postings[name])
		if err != nil {
			return nil, nil, fmt.Errorf("postings %q: %w", name, err)
		}
		if len(blob) > math.MaxUint32 {
			return nil, nil, fmt.Errorf("postings %q exceed uint32 length", name)
		}
		recs = append(recs, rec{
			name:    name,
			off:     uint64(len(posts)),
			length:  uint32(len(blob)),
			docFreq: uint32(len(postings[name])),
		})
		posts = append(posts, blob...)
	}

	terms = append(terms, termsMagic...)
	terms = appendU32(terms, formatVersion)
	terms = appendU32(terms, uint32(len(recs)))
	for _, rec := range recs {
		terms = appendU32(terms, uint32(len(rec.name)))
		terms = append(terms, rec.name...)
		terms = appendU64(terms, rec.off)
		terms = appendU32(terms, rec.length)
		terms = appendU32(terms, rec.docFreq)
	}
	return terms, posts, nil
}

type termRef struct {
	offset uint64
	length uint32
	df     uint32
}

func decodeTermDictionary(terms, posts []byte) (map[string][]docPosting, error) {
	if err := checkHeader(posts, postingsMagic); err != nil {
		return nil, err
	}
	refs, err := decodeTermRefs(terms, uint64(len(posts)))
	if err != nil {
		return nil, err
	}
	out := make(map[string][]docPosting, len(refs))
	for name, ref := range refs {
		list, err := decodePostings(posts[ref.offset : ref.offset+uint64(ref.length)])
		if err != nil {
			return nil, fmt.Errorf("term %q: %w", name, err)
		}
		if uint32(len(list)) != ref.df {
			return nil, fmt.Errorf("term %q: df %d, decoded %d", name, ref.df, len(list))
		}
		out[name] = list
	}
	return out, nil
}

func decodeTermRefs(terms []byte, postingsSize uint64) (map[string]termRef, error) {
	if err := checkHeader(terms, termsMagic); err != nil {
		return nil, err
	}
	b := terms[8:]
	var err error
	var count uint32
	count, b, err = takeU32(b)
	if err != nil {
		return nil, err
	}
	if uint64(count) > uint64(len(b))/20 {
		return nil, fmt.Errorf("term count too large")
	}
	out := make(map[string]termRef, count)
	var prev string
	for i := uint32(0); i < count; i++ {
		// Use = rather than := so the cursor b stays in this function's block.
		// A := inside the loop would shadow b and the next term would be read
		// from the start of the previous record.
		var nameLen, length, df uint32
		var off uint64
		var rest []byte
		nameLen, rest, err = takeU32(b)
		if err != nil {
			return nil, err
		}
		if int(nameLen) > len(rest) {
			return nil, fmt.Errorf("term name truncated")
		}
		name := string(rest[:nameLen])
		b = rest[nameLen:]
		off, b, err = takeU64(b)
		if err != nil {
			return nil, err
		}
		length, b, err = takeU32(b)
		if err != nil {
			return nil, err
		}
		df, b, err = takeU32(b)
		if err != nil {
			return nil, err
		}
		if i > 0 && name <= prev {
			return nil, fmt.Errorf("terms are not strictly sorted at %q", name)
		}
		prev = name
		if off < 8 || off > postingsSize || uint64(length) > postingsSize-off || length == 0 {
			return nil, fmt.Errorf("term %q postings offset out of range", name)
		}
		out[name] = termRef{offset: off, length: length, df: df}
	}
	if len(b) != 0 {
		return nil, fmt.Errorf("trailing bytes in terms.bin")
	}
	return out, nil
}

func encodePostings(list []docPosting) ([]byte, error) {
	var buf []byte
	buf = binary.AppendUvarint(buf, uint64(len(list)))
	var prevDoc uint32
	for i, p := range list {
		if len(p.pos) == 0 {
			return nil, fmt.Errorf("doc %d has no positions", p.doc)
		}
		var delta uint64
		if i == 0 {
			delta = uint64(p.doc)
		} else {
			if p.doc <= prevDoc {
				return nil, fmt.Errorf("docIDs are not strictly increasing")
			}
			delta = uint64(p.doc - prevDoc)
		}
		prevDoc = p.doc
		buf = binary.AppendUvarint(buf, delta)
		buf = binary.AppendUvarint(buf, uint64(len(p.pos)))
		var prevPos uint32
		for j, pos := range p.pos {
			var pd uint64
			if j == 0 {
				pd = uint64(pos)
			} else {
				if pos <= prevPos {
					return nil, fmt.Errorf("doc %d positions are not strictly increasing", p.doc)
				}
				pd = uint64(pos - prevPos)
			}
			prevPos = pos
			buf = binary.AppendUvarint(buf, pd)
		}
	}
	return buf, nil
}

func decodePostings(b []byte) ([]docPosting, error) {
	n, k := binary.Uvarint(b)
	if k <= 0 {
		return nil, fmt.Errorf("postings count")
	}
	b = b[k:]
	if n > uint64(len(b)) {
		return nil, fmt.Errorf("postings count too large")
	}
	out := make([]docPosting, 0, n)
	var prevDoc uint32
	for i := uint64(0); i < n; i++ {
		d, k := binary.Uvarint(b)
		if k <= 0 {
			return nil, fmt.Errorf("postings delta")
		}
		b = b[k:]
		if d > math.MaxUint32 {
			return nil, fmt.Errorf("postings delta overflows uint32")
		}
		var doc uint32
		if i == 0 {
			doc = uint32(d)
		} else {
			if d == 0 {
				return nil, fmt.Errorf("postings delta must be positive")
			}
			doc = prevDoc + uint32(d)
			if doc <= prevDoc {
				return nil, fmt.Errorf("postings docID overflow")
			}
		}
		prevDoc = doc
		freq, k := binary.Uvarint(b)
		if k <= 0 {
			return nil, fmt.Errorf("doc %d position count", doc)
		}
		b = b[k:]
		if freq == 0 || freq > uint64(len(b)) {
			return nil, fmt.Errorf("doc %d position count %d", doc, freq)
		}
		pos := make([]uint32, 0, freq)
		var prevPos uint32
		for j := uint64(0); j < freq; j++ {
			pd, k := binary.Uvarint(b)
			if k <= 0 {
				return nil, fmt.Errorf("doc %d position", doc)
			}
			b = b[k:]
			if pd > math.MaxUint32 {
				return nil, fmt.Errorf("doc %d position overflows uint32", doc)
			}
			var at uint32
			if j == 0 {
				at = uint32(pd)
			} else {
				if pd == 0 {
					return nil, fmt.Errorf("doc %d position delta must be positive", doc)
				}
				at = prevPos + uint32(pd)
				if at <= prevPos {
					return nil, fmt.Errorf("doc %d position overflow", doc)
				}
			}
			prevPos = at
			pos = append(pos, at)
		}
		out = append(out, docPosting{doc: doc, pos: pos})
	}
	if len(b) != 0 {
		return nil, fmt.Errorf("trailing bytes in postings")
	}
	return out, nil
}

func encodeDocs(docs []map[string]string) (docsBin, docsIdx []byte, err error) {
	docsBin = append(docsBin, docsMagic...)
	docsBin = appendU32(docsBin, formatVersion)
	docsBin = appendU32(docsBin, uint32(len(docs)))

	docsIdx = append(docsIdx, docsIdxMagic...)
	docsIdx = appendU32(docsIdx, formatVersion)
	docsIdx = appendU32(docsIdx, uint32(len(docs)))

	for i, doc := range docs {
		payload, err := json.Marshal(doc)
		if err != nil {
			return nil, nil, fmt.Errorf("doc %d: %w", i, err)
		}
		off := uint64(len(docsBin))
		docsBin = binary.AppendUvarint(docsBin, uint64(len(payload)))
		docsBin = append(docsBin, payload...)
		docsIdx = appendU32(docsIdx, uint32(i))
		docsIdx = appendU64(docsIdx, off)
	}
	return docsBin, docsIdx, nil
}

func decodeDocsIndex(b []byte) ([]uint64, error) {
	if err := checkHeader(b, docsIdxMagic); err != nil {
		return nil, err
	}
	rest := b[8:]
	count, rest, err := takeU32(rest)
	if err != nil {
		return nil, err
	}
	if uint64(count) > uint64(len(rest))/12 {
		return nil, fmt.Errorf("docs.idx count too large")
	}
	off := make([]uint64, count)
	for i := uint32(0); i < count; i++ {
		id, next, err := takeU32(rest)
		if err != nil {
			return nil, err
		}
		pos, next, err := takeU64(next)
		if err != nil {
			return nil, err
		}
		rest = next
		if id != i {
			return nil, fmt.Errorf("docs.idx id %d, want %d", id, i)
		}
		off[i] = pos
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("trailing bytes in docs.idx")
	}
	return off, nil
}

func checkHeader(b []byte, magic string) error {
	if len(b) < 8 {
		return fmt.Errorf("short header")
	}
	if string(b[:4]) != magic {
		return fmt.Errorf("magic %q, want %s", b[:4], magic)
	}
	ver := binary.LittleEndian.Uint32(b[4:8])
	if ver != formatVersion {
		return fmt.Errorf("version %d, want %d", ver, formatVersion)
	}
	return nil
}

func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync %s: %w", filepath.Base(path), err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	return nil
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	err = d.Sync()
	_ = d.Close()
	return err
}

func appendU32(b []byte, v uint32) []byte {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], v)
	return append(b, buf[:]...)
}

func appendU64(b []byte, v uint64) []byte {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], v)
	return append(b, buf[:]...)
}

func takeU32(b []byte) (uint32, []byte, error) {
	if len(b) < 4 {
		return 0, nil, fmt.Errorf("truncated uint32")
	}
	return binary.LittleEndian.Uint32(b[:4]), b[4:], nil
}

func takeU64(b []byte) (uint64, []byte, error) {
	if len(b) < 8 {
		return 0, nil, fmt.Errorf("truncated uint64")
	}
	return binary.LittleEndian.Uint64(b[:8]), b[8:], nil
}
