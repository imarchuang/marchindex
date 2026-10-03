package index

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestDeltaEncodeRoundTrip(t *testing.T) {
	ids := []uint32{0, 1, 2, 10, 1000}
	blob, err := encodePostings(ids)
	if err != nil {
		t.Fatal(err)
	}
	// count=5, then 0, +1, +1, +8, +990. Each of those fits in one uvarint byte
	// except 990 (two bytes: 0xDE 0x07).
	if got, want := blob, []byte{5, 0, 1, 1, 8, 0xDE, 0x07}; string(got) != string(want) {
		t.Fatalf("encoding = %v, want %v", got, want)
	}
	got, err := decodePostings(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !equalU32(got, ids) {
		t.Fatalf("decoded = %v, want %v", got, ids)
	}

	if _, err := encodePostings([]uint32{3, 3}); err == nil {
		t.Fatal("expected strictly increasing docIDs")
	}
	if _, err := decodePostings([]byte{1, 1, 0}); err == nil {
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
	if !equalU32(decoded["level:error"], []uint32{0}) {
		t.Fatalf("level:error postings = %v, want local doc 0", decoded["level:error"])
	}
	if !equalU32(decoded["service:api"], []uint32{0, 1}) {
		t.Fatalf("service:api postings = %v", decoded["service:api"])
	}
	// Both docs share service:api, so the on-disk list is doc 0 then delta 1.
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
	ids, err := decodePostings(blob)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}
