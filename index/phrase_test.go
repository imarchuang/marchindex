package index

import (
	"testing"
)

func TestPhraseSearch(t *testing.T) {
	_, idx := newTestIndex(t)
	docs := []struct {
		id     string
		fields map[string]string
	}{
		{"adj", map[string]string{"level": "error", "message": "timeout calling db"}},
		{"swap", map[string]string{"level": "error", "message": "calling timeout"}},
		{"gap", map[string]string{"level": "info", "message": "timeout db"}},
		{"short", map[string]string{"level": "info", "message": "timeout a db"}},
		{"punct", map[string]string{"level": "info", "message": "timeout, db"}},
		{"repeat", map[string]string{"level": "warn", "message": "foo foo"}},
		{"apart", map[string]string{"level": "warn", "message": "foo bar foo"}},
		{"other", map[string]string{"level": "error", "service": "timeout calling", "message": "request ok"}},
	}
	for _, doc := range docs {
		mustIndex(t, idx, doc.id, doc.fields)
	}

	tests := []struct {
		name string
		q    string
		want []string
	}{
		{name: "adjacent", q: `"timeout calling"`, want: []string{"adj"}},
		{name: "case", q: `"Timeout Calling"`, want: []string{"adj"}},
		{name: "later pair", q: `"calling db"`, want: []string{"adj"}},
		{name: "adjacent across punctuation", q: `"timeout db"`, want: []string{"gap", "punct"}},
		{name: "reversed", q: `"calling timeout"`, want: []string{"swap"}},
		{name: "field phrase", q: `message:"timeout calling"`, want: []string{"adj"}},
		{name: "other field does not match message phrase", q: `"timeout calling"`, want: []string{"adj"}},
		{name: "service phrase", q: `service:"timeout calling"`, want: []string{"other"}},
		{name: "and with phrase", q: `level:error AND "timeout calling"`, want: []string{"adj"}},
		{name: "and excludes info", q: `level:info AND "timeout db"`, want: []string{"gap", "punct"}},
		{name: "repeated term", q: `"foo foo"`, want: []string{"repeat"}},
		{name: "missing phrase", q: `"disk full"`, want: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := idx.Search(tt.q, 10)
			if err != nil {
				t.Fatalf("Search(%q): %v", tt.q, err)
			}
			if !equalStrings(hitIDs(res.Hits), tt.want) {
				t.Fatalf("Search(%q) = %v, want %v", tt.q, hitIDs(res.Hits), tt.want)
			}
			node, err := parseQuery(tt.q)
			if err != nil {
				t.Fatal(err)
			}
			if res.PostingsLookups != node.termCount() {
				t.Fatalf("postings_lookups = %d, want %d", res.PostingsLookups, node.termCount())
			}
		})
	}

	// "timeout a db" keeps a position for the dropped "a", so the indexed
	// terms are two apart and the adjacent phrase misses this document.
	res, err := idx.Search(`"timeout db"`, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range hitIDs(res.Hits) {
		if id == "short" {
			t.Fatal(`"timeout db" matched "timeout a db"`)
		}
	}
}

func TestPhraseSurvivesFlushAndMerge(t *testing.T) {
	_, idx := newTestIndex(t)
	mustIndex(t, idx, "a", map[string]string{"message": "timeout calling db"})
	if _, err := idx.Flush(); err != nil {
		t.Fatal(err)
	}
	mustIndex(t, idx, "b", map[string]string{"message": "timeout db"})
	if _, err := idx.Flush(); err != nil {
		t.Fatal(err)
	}
	res, err := idx.Search(`"timeout calling"`, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(res.Hits), []string{"a"}) {
		t.Fatalf("flushed hits = %v", hitIDs(res.Hits))
	}
	if _, err := idx.ForceMerge(); err != nil {
		t.Fatal(err)
	}
	res, err = idx.Search(`"calling db"`, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(res.Hits), []string{"a"}) || res.Hits[0]["_doc"] != "0" {
		t.Fatalf("merged hits = %#v", res.Hits)
	}
	res, err = idx.Search(`"timeout db"`, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(res.Hits), []string{"b"}) {
		t.Fatalf("second doc = %v", hitIDs(res.Hits))
	}
}
