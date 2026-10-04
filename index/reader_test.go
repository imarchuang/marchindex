package index

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSearchReadsOnlySelectedRanges(t *testing.T) {
	_, idx := newTestIndex(t)
	for i := 0; i < 200; i++ {
		message := "ordinary common"
		if i == 0 {
			message = "timeout calling timeout"
		}
		mustIndex(t, idx, fmt.Sprint(i), map[string]string{
			"message": message,
			"payload": strings.Repeat("padding ", 512),
			"tag":     fmt.Sprintf("tag%04d", i),
		})
	}
	flushed, err := idx.Flush()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(idx.SegmentsDir(), flushed.Segment)
	terms := mustRead(t, filepath.Join(dir, "terms.bin"))
	posts := mustRead(t, filepath.Join(dir, "postings.bin"))
	docs := mustRead(t, filepath.Join(dir, "docs.bin"))
	offsets, err := decodeDocsIndex(mustRead(t, filepath.Join(dir, "docs.idx")))
	if err != nil {
		t.Fatal(err)
	}
	firstDocBytes := int64(offsets[1] - offsets[0])
	metadata := int64(8 + 12) // postings/docs headers
	for _, path := range []string{idx.SegmentsFilePath(), filepath.Join(dir, "meta.json"), filepath.Join(dir, "terms.bin"), filepath.Join(dir, "docs.idx")} {
		metadata += int64(len(mustRead(t, path)))
	}
	for _, tt := range []struct {
		q     string
		limit int
		tf    bool
		keys  []string
		hits  int
	}{
		{q: "timeout", limit: 1, keys: []string{"message:timeout"}, hits: 1},
		{q: "timeout OR timeout", limit: 1, keys: []string{"message:timeout"}, hits: 1},
		{q: `"timeout calling"`, limit: 1, keys: []string{"message:timeout", "message:calling"}, hits: 1},
		{q: "timeot~1", limit: 1, keys: []string{"message:timeout"}, hits: 1},
		{q: "timeout~0", limit: 1, keys: []string{"message:timeout"}, hits: 1},
		{q: "timeout OR ordinary", limit: 1, tf: true, keys: []string{"message:timeout", "message:ordinary"}, hits: 1},
		{q: "timeout", limit: 0, keys: []string{"message:timeout"}},
		{q: "missing", limit: 10},
		{q: "zzzzzz~1", limit: 10},
	} {
		t.Run(fmt.Sprintf("%s/limit%d/tf%v", tt.q, tt.limit, tt.tf), func(t *testing.T) {
			search := idx.Search
			if tt.tf {
				search = idx.SearchTF
			}
			res, err := search(tt.q, tt.limit)
			if err != nil {
				t.Fatal(err)
			}
			var postBytes int64
			for _, key := range tt.keys {
				_, length := termSlice(t, terms, key)
				postBytes += int64(length)
			}
			want := SearchIO{MetadataBytesRead: metadata, PostingsBytesRead: postBytes, DocsBytesRead: firstDocBytes * int64(tt.hits), PostingsDecoded: len(tt.keys)}
			if res.IO != want {
				t.Fatalf("io = %+v, want %+v", res.IO, want)
			}
			t.Logf("read metadata=%d; postings=%d/%d; docs=%d/%d; lists=%d",
				res.IO.MetadataBytesRead, res.IO.PostingsBytesRead, len(posts),
				res.IO.DocsBytesRead, len(docs), res.IO.PostingsDecoded)
			if len(res.Hits) != tt.hits {
				t.Fatalf("hits = %v", res.Hits)
			}
			if tt.hits > 0 && res.Hits[0]["_id"] != "0" {
				t.Fatalf("wrong hit: %v", res.Hits)
			}
			if tt.tf && res.Hits[0]["_score"] != "2" {
				t.Fatalf("score = %s", res.Hits[0]["_score"])
			}
			if res.IO.DocsBytesRead >= int64(len(docs))/100 || res.IO.PostingsBytesRead >= int64(len(posts))/100 {
				t.Fatalf("selective query read too much: %+v (docs=%d posts=%d)", res.IO, len(docs), len(posts))
			}
		})
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Corrupt unselected payloads: a selective query must not decode either one.
// Selecting them must still surface an error rather than silently dropping hits.
func TestSearchDoesNotDecodeUnrelatedPayloads(t *testing.T) {
	for _, kind := range []string{"postings", "docs"} {
		t.Run(kind, func(t *testing.T) {
			_, idx := newTestIndex(t)
			mustIndex(t, idx, "good", map[string]string{"message": "timeout"})
			mustIndex(t, idx, "bad", map[string]string{"message": "unrelated"})
			fl, err := idx.Flush()
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(idx.SegmentsDir(), fl.Segment)
			path := filepath.Join(dir, kind+".bin")
			b := mustRead(t, path)
			if kind == "postings" {
				off, length := termSlice(t, mustRead(t, filepath.Join(dir, "terms.bin")), "message:unrelated")
				for i := off; i < off+uint64(length); i++ {
					b[i] = 0xff
				}
			} else {
				off, err := decodeDocsIndex(mustRead(t, filepath.Join(dir, "docs.idx")))
				if err != nil {
					t.Fatal(err)
				}
				_, k := binary.Uvarint(b[off[1]:])
				b[off[1]+uint64(k)] = '!'
			}
			if err := os.WriteFile(path, b, 0644); err != nil {
				t.Fatal(err)
			}
			res, err := idx.Search("timeout", 10)
			if err != nil || len(res.Hits) != 1 {
				t.Fatalf("unrelated payload read: %+v, %v", res, err)
			}
			if _, err := idx.Search("unrelated", 10); err == nil {
				t.Fatal("expected selected payload corruption error")
			}
		})
	}
}

func TestSegmentReaderRejectsBadMetadata(t *testing.T) {
	for _, kind := range []string{"postings-header", "docs-header", "postings-range", "docs-offset", "docs-count", "terms-count", "deleted"} {
		t.Run(kind, func(t *testing.T) {
			_, idx := newTestIndex(t)
			mustIndex(t, idx, "one", map[string]string{"message": "timeout"})
			fl, err := idx.Flush()
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(idx.SegmentsDir(), fl.Segment)
			file := ""
			var b []byte
			switch kind {
			case "postings-header", "docs-header":
				file = strings.TrimSuffix(kind, "-header") + ".bin"
				b = []byte("bad")
			case "postings-range":
				file = "terms.bin"
				b = mustRead(t, filepath.Join(dir, file))
				nameLen := int(binary.LittleEndian.Uint32(b[12:16]))
				binary.LittleEndian.PutUint64(b[16+nameLen:], ^uint64(0))
			case "docs-offset":
				file = "docs.idx"
				b = mustRead(t, filepath.Join(dir, file))
				binary.LittleEndian.PutUint64(b[16:], ^uint64(0))
			case "docs-count", "terms-count":
				file = "docs.idx"
				if kind == "terms-count" {
					file = "terms.bin"
				}
				b = mustRead(t, filepath.Join(dir, file))
				binary.LittleEndian.PutUint32(b[8:], ^uint32(0))
			case "deleted":
				file = "deleted.bits"
				b = []byte("bad")
			}
			if err := os.WriteFile(filepath.Join(dir, file), b, 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := idx.Search("timeout", 1); err == nil {
				t.Fatal("expected metadata error")
			}
		})
	}
}

func TestSegmentReaderShortReadAndClose(t *testing.T) {
	_, idx := newTestIndex(t)
	mustIndex(t, idx, "one", map[string]string{"message": "timeout"})
	fl, err := idx.Flush()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(idx.SegmentsDir(), fl.Segment)
	var stats SearchIO
	r, err := openSegmentReader(dir, fl.Segment, &stats)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	if err := os.Truncate(filepath.Join(dir, "docs.bin"), 12); err != nil {
		t.Fatal(err)
	}
	if _, err := r.fetch(0); err == nil {
		t.Fatal("expected short doc read error")
	}
	if err := os.Truncate(filepath.Join(dir, "postings.bin"), 8); err != nil {
		t.Fatal(err)
	}
	n, err := parseQuery("timeout")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.queryPostings(n); err == nil {
		t.Fatal("expected short postings read error")
	}
	r.close()
	if _, err := r.store.Stat(); err == nil {
		t.Fatal("docs handle still open")
	}
	if _, err := r.posts.Stat(); err == nil {
		t.Fatal("postings handle still open")
	}
}

func TestLazySearchAcrossDeleteMergeAndRestart(t *testing.T) {
	mgr, idx := newTestIndex(t)
	mustIndex(t, idx, "old", map[string]string{"message": "timeout calling"})
	if _, err := idx.Flush(); err != nil {
		t.Fatal(err)
	}
	// Warm search before deletion; no stale bitsets may survive into later calls.
	if _, err := idx.Search("timeout", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Delete("old"); err != nil {
		t.Fatal(err)
	}
	deleted, err := idx.Search("timeout", 10)
	if err != nil || len(deleted.Hits) != 0 || deleted.IO.DocsBytesRead != 0 {
		t.Fatalf("deleted: %+v %v", deleted, err)
	}
	mustIndex(t, idx, "new", map[string]string{"message": "timeout calling"})
	if _, err := idx.Flush(); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				r, err := idx.Search(`"timeout calling" OR timeot~1`, 1)
				if err != nil || len(r.Hits) != 1 || r.Hits[0]["_id"] != "new" {
					t.Errorf("concurrent search: %+v %v", r, err)
					return
				}
			}
		}()
	}
	if _, err := idx.ForceMerge(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	restarted, err := NewManager(mgr.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	again, err := restarted.GetIndex(idx.Name())
	if err != nil {
		t.Fatal(err)
	}
	r, err := again.SearchTF("timeot~1", 1)
	if err != nil || len(r.Hits) != 1 || r.Hits[0]["_id"] != "new" || r.IO.PostingsDecoded != 1 {
		t.Fatalf("restart: %+v %v", r, err)
	}
}
