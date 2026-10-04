package index

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDeleteSkipsUntilMergeDrops(t *testing.T) {
	mgr, idx := newTestIndex(t)
	mustIndex(t, idx, "a", map[string]string{"service": "api", "level": "error", "message": "timeout calling db"})
	if _, err := idx.Flush(); err != nil {
		t.Fatal(err)
	}
	mustIndex(t, idx, "b", map[string]string{"service": "api", "level": "info", "message": "request ok"})
	if _, err := idx.Flush(); err != nil {
		t.Fatal(err)
	}

	res, err := idx.Delete("a")
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("deleted = %+v", res)
	}
	hits, err := idx.Search("level:error", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits.Hits) != 0 {
		t.Fatalf("deleted doc still visible: %#v", hits.Hits)
	}
	if hits.PostingsLookups != 2 {
		t.Fatalf("postings_lookups = %d, want one lookup in each committed segment", hits.PostingsLookups)
	}

	// The bitset hides the doc. The immutable postings list still contains it.
	terms, err := os.ReadFile(filepath.Join(idx.SegmentsDir(), "seg-000001", "terms.bin"))
	if err != nil {
		t.Fatal(err)
	}
	posts, err := os.ReadFile(filepath.Join(idx.SegmentsDir(), "seg-000001", "postings.bin"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeTermDictionary(terms, posts)
	if err != nil {
		t.Fatal(err)
	}
	if !equalU32(decoded["level:error"], []uint32{0}) {
		t.Fatalf("postings after delete = %v, want the doc kept until merge", decoded["level:error"])
	}
	if _, err := os.Stat(filepath.Join(idx.SegmentsDir(), "seg-000001", "deleted.bits")); err != nil {
		t.Fatal(err)
	}

	still, err := idx.Search("level:info", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(still.Hits), []string{"b"}) {
		t.Fatalf("live doc = %v", hitIDs(still.Hits))
	}

	merged, err := idx.ForceMerge()
	if err != nil {
		t.Fatal(err)
	}
	if !merged.Merged || merged.SegmentsBefore != 2 || merged.SegmentsAfter != 1 || merged.Dropped != 1 || merged.Docs != 1 {
		t.Fatalf("merge = %+v", merged)
	}
	if _, err := os.Stat(filepath.Join(idx.SegmentsDir(), "seg-000001")); !os.IsNotExist(err) {
		t.Fatalf("input segment still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(idx.SegmentsDir(), merged.Segment, "deleted.bits")); !os.IsNotExist(err) {
		t.Fatalf("merged segment should not carry deleted.bits: %v", err)
	}
	terms, err = os.ReadFile(filepath.Join(idx.SegmentsDir(), merged.Segment, "terms.bin"))
	if err != nil {
		t.Fatal(err)
	}
	posts, err = os.ReadFile(filepath.Join(idx.SegmentsDir(), merged.Segment, "postings.bin"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = decodeTermDictionary(terms, posts)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["level:error"]; ok {
		t.Fatal("merged postings still contain the deleted term")
	}
	if !equalU32(decoded["level:info"], []uint32{0}) {
		t.Fatalf("level:info = %v", decoded["level:info"])
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
	gone, err := again.Search("level:error", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(gone.Hits) != 0 {
		t.Fatalf("deleted doc returned after restart: %#v", gone.Hits)
	}
	kept, err := again.Search("service:api", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(kept.Hits), []string{"b"}) {
		t.Fatalf("kept = %v", hitIDs(kept.Hits))
	}

	if _, err := again.Delete("a"); !errors.Is(err, ErrDocNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
}

func TestForceMergeCompactsSegments(t *testing.T) {
	_, idx := newTestIndex(t)
	ids := []string{"a", "b", "c", "d"}
	for _, id := range ids {
		mustIndex(t, idx, id, map[string]string{"level": "error", "service": "api", "message": "timeout " + id})
		if _, err := idx.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	// The third flush crosses autoMergeAt, so four flushes do not leave four segments.
	cp, err := readCommit(idx.SegmentsFilePath())
	if err != nil {
		t.Fatal(err)
	}
	if len(cp.Segments) != 2 {
		t.Fatalf("segments before force = %+v, want the auto-merged segment plus the fourth flush", cp.Segments)
	}

	merged, err := idx.ForceMerge()
	if err != nil {
		t.Fatal(err)
	}
	if !merged.Merged || merged.SegmentsBefore != 2 || merged.SegmentsAfter != 1 || merged.Docs != 4 || merged.Dropped != 0 {
		t.Fatalf("merge = %+v", merged)
	}
	res, err := idx.Search("level:error", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(res.Hits), ids) {
		t.Fatalf("hits = %v", hitIDs(res.Hits))
	}
	if res.SegmentsSearched != 1 || res.Hits[0]["_seg"] != merged.Segment || res.Hits[0]["_doc"] != "0" {
		t.Fatalf("hits = %#v", res.Hits)
	}

	again, err := idx.ForceMerge()
	if err != nil {
		t.Fatal(err)
	}
	if again.Merged || again.SegmentsAfter != 1 {
		t.Fatalf("second merge = %+v", again)
	}
}

func TestAutoMergeAfterThirdFlush(t *testing.T) {
	_, idx := newTestIndex(t)
	for _, id := range []string{"a", "b", "c"} {
		mustIndex(t, idx, id, map[string]string{"level": "error", "message": "timeout"})
		if _, err := idx.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	cp, err := readCommit(idx.SegmentsFilePath())
	if err != nil {
		t.Fatal(err)
	}
	if len(cp.Segments) != 1 || cp.Segments[0].ID != "seg-000004" {
		t.Fatalf("commit = %+v, want the merged segment only", cp)
	}
	for _, old := range []string{"seg-000001", "seg-000002", "seg-000003"} {
		if _, err := os.Stat(filepath.Join(idx.SegmentsDir(), old)); !os.IsNotExist(err) {
			t.Fatalf("%s still on disk: %v", old, err)
		}
	}
	res, err := idx.Search("level:error", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(res.Hits), []string{"a", "b", "c"}) {
		t.Fatalf("hits = %v", hitIDs(res.Hits))
	}
}

func TestDeleteRAMAndEverySegmentCopy(t *testing.T) {
	_, idx := newTestIndex(t)
	mustIndex(t, idx, "1", map[string]string{"level": "error", "message": "timeout"})
	if _, err := idx.Flush(); err != nil {
		t.Fatal(err)
	}
	mustIndex(t, idx, "1", map[string]string{"level": "info", "message": "request ok"})

	res, err := idx.Delete("1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 2 {
		t.Fatalf("deleted = %+v, want the segment copy and the RAM copy", res)
	}
	left, err := idx.Search("level:error OR level:info", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left.Hits) != 0 {
		t.Fatalf("copies still visible: %#v", left.Hits)
	}
}

func TestMergeDropsEveryDeletedDoc(t *testing.T) {
	_, idx := newTestIndex(t)
	mustIndex(t, idx, "only", map[string]string{"level": "error", "message": "timeout"})
	if _, err := idx.Flush(); err != nil {
		t.Fatal(err)
	}
	mustIndex(t, idx, "other", map[string]string{"level": "info", "message": "ok"})
	if _, err := idx.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Delete("only"); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Delete("other"); err != nil {
		t.Fatal(err)
	}
	merged, err := idx.ForceMerge()
	if err != nil {
		t.Fatal(err)
	}
	if !merged.Merged || merged.Docs != 0 || merged.Dropped != 2 || merged.Segment != "" || merged.SegmentsAfter != 0 {
		t.Fatalf("merge = %+v", merged)
	}
	res, err := idx.Search("level:error OR level:info", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 0 || res.SegmentsSearched != 0 {
		t.Fatalf("search = %+v", res)
	}
}
