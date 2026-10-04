package index

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestDeltaEncodeRoundTrip(t *testing.T) {
	list := []docPosting{
		{doc: 0, pos: []uint32{0}},
		{doc: 1, pos: []uint32{0, 2}},
		{doc: 2, pos: []uint32{1}},
	}
	blob, err := encodePostings(list)
	if err != nil {
		t.Fatal(err)
	}
	// count=3
	// doc 0, freq 1, pos 0
	// delta 1, freq 2, pos 0, delta 2
	// delta 1, freq 1, pos 1
	want := []byte{3, 0, 1, 0, 1, 2, 0, 2, 1, 1, 1}
	if string(blob) != string(want) {
		t.Fatalf("encoding = %v, want %v", blob, want)
	}
	got, err := decodePostings(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !equalU32(postingIDs(got), []uint32{0, 1, 2}) {
		t.Fatalf("docs = %v", postingIDs(got))
	}
	if !equalU32(got[1].pos, []uint32{0, 2}) {
		t.Fatalf("doc 1 positions = %v", got[1].pos)
	}

	if _, err := encodePostings([]docPosting{{doc: 3, pos: []uint32{0}}, {doc: 3, pos: []uint32{1}}}); err == nil {
		t.Fatal("expected strictly increasing docIDs")
	}
	if _, err := decodePostings([]byte{1, 0, 1, 0, 9}); err == nil {
		t.Fatal("expected trailing byte error")
	}
}

func TestFlushCommitAndRestart(t *testing.T) {
	mgr, idx := newTestIndex(t)
	mustIndex(t, idx, "", map[string]string{
		"service": "api",
		"level":   "error",
		"message": "timeout calling db",
	})
	mustIndex(t, idx, "", map[string]string{
		"service": "api",
		"level":   "info",
		"message": "request ok",
	})

	flushed, err := idx.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if !flushed.Flushed || flushed.Segment != "seg-000001" || flushed.Docs != 2 || flushed.Generation != 1 {
		t.Fatalf("flush = %+v", flushed)
	}
	if flushed.Terms == 0 {
		t.Fatal("expected terms in the segment")
	}

	empty, err := idx.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if empty.Flushed || empty.Segment != "" || empty.Generation != 1 {
		t.Fatalf("empty flush = %+v", empty)
	}

	segDir := filepath.Join(idx.SegmentsDir(), "seg-000001")
	for _, name := range []string{"meta.json", "terms.bin", "postings.bin", "docs.bin", "docs.idx"} {
		if _, err := os.Stat(filepath.Join(segDir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	posts, err := os.ReadFile(filepath.Join(segDir, "postings.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(posts[:4]) != postingsMagic {
		t.Fatalf("postings magic = %q", posts[:4])
	}
	terms, err := os.ReadFile(filepath.Join(segDir, "terms.bin"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeTermDictionary(terms, posts)
	if err != nil {
		t.Fatal(err)
	}
	if !equalU32(postingIDs(decoded["level:error"]), []uint32{0}) {
		t.Fatalf("level:error postings = %v, want local doc 0", postingIDs(decoded["level:error"]))
	}
	if !equalU32(postingIDs(decoded["service:api"]), []uint32{0, 1}) {
		t.Fatalf("service:api postings = %v", postingIDs(decoded["service:api"]))
	}
	// Both docs contain service:api once, at position 0. The docID list is
	// doc 0 then delta 1, and each doc stores freq 1, pos 0.
	off, length := termSlice(t, terms, "service:api")
	blob := posts[off : off+uint64(length)]
	if !equalU32(mustDecode(t, blob), []uint32{0, 1}) {
		t.Fatalf("raw service:api blob %v decoded wrong", blob)
	}

	// A directory that is not in segments.json must not be searched.
	if err := os.Mkdir(filepath.Join(idx.SegmentsDir(), "seg-000099"), 0755); err != nil {
		t.Fatal(err)
	}

	res, err := idx.Search("level:error AND service:api", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(res.Hits), []string{"1"}) {
		t.Fatalf("hits = %v", hitIDs(res.Hits))
	}
	if res.Hits[0]["_seg"] != "seg-000001" || res.Hits[0]["_doc"] != "0" {
		t.Fatalf("hit = %#v, want seg-000001 doc 0", res.Hits[0])
	}
	if res.Hits[0]["message"] != "timeout calling db" {
		t.Fatalf("stored message = %q", res.Hits[0]["message"])
	}
	if res.SegmentsSearched != 1 {
		t.Fatalf("segments_searched = %d", res.SegmentsSearched)
	}
	if res.PostingsLookups != 2 {
		t.Fatalf("postings_lookups = %d, want 2 (one segment, RAM empty)", res.PostingsLookups)
	}
	if res.DocsExamined != 1 {
		t.Fatalf("docs_examined = %d", res.DocsExamined)
	}

	list, err := mgr.ListIndices()
	if err != nil {
		t.Fatal(err)
	}
	if list[0].SegmentCount != 1 {
		t.Fatalf("segment_count = %d", list[0].SegmentCount)
	}

	restarted, err := NewManager(mgr.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	again, err := restarted.GetIndex("logs")
	if err != nil {
		t.Fatal(err)
	}
	res, err = again.Search("level:error AND service:api", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(res.Hits), []string{"1"}) {
		t.Fatalf("restart hits = %v", hitIDs(res.Hits))
	}
	if res.SegmentsSearched != 1 || res.PostingsLookups != 2 {
		t.Fatalf("restart stats = %+v", res)
	}
}

func TestFlushThenRAMIsNearRealTime(t *testing.T) {
	_, idx := newTestIndex(t)
	mustIndex(t, idx, "a", map[string]string{"service": "api", "level": "error", "message": "timeout calling db"})
	if _, err := idx.Flush(); err != nil {
		t.Fatal(err)
	}
	mustIndex(t, idx, "b", map[string]string{"service": "worker", "level": "error", "message": "disk full"})

	res, err := idx.Search("level:error", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(res.Hits), []string{"a", "b"}) {
		t.Fatalf("hits = %v", hitIDs(res.Hits))
	}
	if res.Hits[0]["_seg"] != "seg-000001" || res.Hits[1]["_seg"] != "_ram" {
		t.Fatalf("segs = %s %s", res.Hits[0]["_seg"], res.Hits[1]["_seg"])
	}
	if res.SegmentsSearched != 1 || res.PostingsLookups != 2 {
		t.Fatalf("stats segments=%d lookups=%d, want 1 and 2", res.SegmentsSearched, res.PostingsLookups)
	}

	second, err := idx.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if second.Segment != "seg-000002" || second.Generation != 2 || second.Docs != 1 {
		t.Fatalf("second flush = %+v", second)
	}
	res, err = idx.Search("level:error", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(res.Hits), []string{"a", "b"}) {
		t.Fatalf("after second flush hits = %v", hitIDs(res.Hits))
	}
	if res.Hits[1]["_seg"] != "seg-000002" || res.Hits[1]["_doc"] != "0" {
		t.Fatalf("second segment hit = %#v", res.Hits[1])
	}
	if res.SegmentsSearched != 2 || res.PostingsLookups != 2 {
		t.Fatalf("stats segments=%d lookups=%d", res.SegmentsSearched, res.PostingsLookups)
	}
}

func TestReindexAfterFlushKeepsOldSegmentHit(t *testing.T) {
	_, idx := newTestIndex(t)
	mustIndex(t, idx, "1", map[string]string{"service": "api", "level": "error", "message": "timeout calling db"})
	if _, err := idx.Flush(); err != nil {
		t.Fatal(err)
	}
	mustIndex(t, idx, "1", map[string]string{"service": "api", "level": "info", "message": "request ok"})

	res, err := idx.Search("service:api", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 2 || res.Hits[0]["_id"] != "1" || res.Hits[1]["_id"] != "1" {
		t.Fatalf("hits = %#v, want the committed copy and the RAM replacement", res.Hits)
	}
	if res.Hits[0]["level"] != "error" || res.Hits[1]["level"] != "info" {
		t.Fatalf("hits = %#v", res.Hits)
	}
}

func termSlice(t *testing.T, terms []byte, want string) (off uint64, length uint32) {
	t.Helper()
	if err := checkHeader(terms, termsMagic); err != nil {
		t.Fatal(err)
	}
	b := terms[8:]
	count := binary.LittleEndian.Uint32(b[:4])
	b = b[4:]
	for i := uint32(0); i < count; i++ {
		nameLen := binary.LittleEndian.Uint32(b[:4])
		b = b[4:]
		name := string(b[:nameLen])
		b = b[nameLen:]
		off = binary.LittleEndian.Uint64(b[:8])
		length = binary.LittleEndian.Uint32(b[8:12])
		b = b[16:]
		if name == want {
			return off, length
		}
	}
	t.Fatalf("term %q not in dictionary", want)
	return 0, 0
}

func mustDecode(t *testing.T, blob []byte) []uint32 {
	t.Helper()
	list, err := decodePostings(blob)
	if err != nil {
		t.Fatal(err)
	}
	return postingIDs(list)
}
