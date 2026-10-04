package index

import "testing"

func TestStatsAndTermFrequency(t *testing.T) {
	_, idx := newTestIndex(t)
	mustIndex(t, idx, "low", map[string]string{"level": "error", "message": "timeout once"})
	mustIndex(t, idx, "high", map[string]string{"level": "error", "message": "timeout timeout timeout"})
	if _, err := idx.Flush(); err != nil {
		t.Fatal(err)
	}
	mustIndex(t, idx, "ram", map[string]string{"level": "info", "message": "timeout"})

	st, err := idx.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Name != "logs" || st.Generation != 1 || st.RamDocs != 1 || st.Docs != 3 || st.Deleted != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if len(st.Segments) != 1 || st.Segments[0].Docs != 2 || st.Segments[0].Live != 2 || st.Segments[0].Terms == 0 || st.Segments[0].Bytes == 0 {
		t.Fatalf("segment stats = %+v", st.Segments)
	}

	plain, err := idx.Search("timeout", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(plain.Hits), []string{"low", "high", "ram"}) {
		t.Fatalf("doc order = %v", hitIDs(plain.Hits))
	}
	if _, ok := plain.Hits[0]["_score"]; ok {
		t.Fatal("default search should not add _score")
	}

	ranked, err := idx.SearchTF("timeout", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(ranked.Hits), []string{"high", "low", "ram"}) {
		t.Fatalf("tf order = %v", hitIDs(ranked.Hits))
	}
	if ranked.Hits[0]["_score"] != "3" || ranked.Hits[1]["_score"] != "1" || ranked.Hits[2]["_score"] != "1" {
		t.Fatalf("scores = %q %q %q", ranked.Hits[0]["_score"], ranked.Hits[1]["_score"], ranked.Hits[2]["_score"])
	}

	if _, err := idx.Delete("low"); err != nil {
		t.Fatal(err)
	}
	st, err = idx.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Deleted != 1 || st.Docs != 2 || st.Segments[0].Live != 1 {
		t.Fatalf("stats after delete = %+v seg %+v", st, st.Segments)
	}

	top, err := idx.SearchTF("timeout", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(top.Hits), []string{"high"}) || top.DocsExamined != 1 {
		t.Fatalf("top = %#v examined %d", top.Hits, top.DocsExamined)
	}
}
