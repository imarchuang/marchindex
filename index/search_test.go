package index

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestPostingsBoolean(t *testing.T) {
	tests := []struct {
		name    string
		a, b    []uint32
		and, or []uint32
	}{
		{name: "overlap", a: []uint32{1, 2, 3}, b: []uint32{2, 3, 4}, and: []uint32{2, 3}, or: []uint32{1, 2, 3, 4}},
		{name: "disjoint", a: []uint32{1, 2}, b: []uint32{3, 4}, and: nil, or: []uint32{1, 2, 3, 4}},
		{name: "empty left", a: nil, b: []uint32{1}, and: nil, or: []uint32{1}},
		{name: "subset", a: []uint32{1, 5}, b: []uint32{1, 2, 5, 9}, and: []uint32{1, 5}, or: []uint32{1, 2, 5, 9}},
		{name: "equal", a: []uint32{4, 8}, b: []uint32{4, 8}, and: []uint32{4, 8}, or: []uint32{4, 8}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := intersect(tt.a, tt.b); !equalU32(got, tt.and) {
				t.Fatalf("intersect = %v, want %v", got, tt.and)
			}
			if got := union(tt.a, tt.b); !equalU32(got, tt.or) {
				t.Fatalf("union = %v, want %v", got, tt.or)
			}
		})
	}
}

func TestBooleanSearch(t *testing.T) {
	mgr, idx := newTestIndex(t)
	docs := []struct {
		id     string
		fields map[string]string
	}{
		{"a1", map[string]string{"service": "api", "level": "error", "message": "timeout calling db"}},
		{"a2", map[string]string{"service": "api", "level": "info", "message": "request ok"}},
		{"w1", map[string]string{"service": "worker", "level": "error", "message": "disk full"}},
		{"a3", map[string]string{"service": "api", "level": "error", "message": "connection reset"}},
		{"d1", map[string]string{"service": "db", "level": "warn", "message": "slow query timeout"}},
		{"sv", map[string]string{"service": "timeout", "level": "info", "message": "hello there"}},
	}
	for _, doc := range docs {
		mustIndex(t, idx, doc.id, doc.fields)
	}
	if err := idx.checkPostings(); err != nil {
		t.Fatal(err)
	}

	// Search through a fresh handle so the manager's shared RAM buffer is what answers.
	idx, err := mgr.GetIndex("logs")
	if err != nil {
		t.Fatalf("GetIndex: %v", err)
	}

	tests := []struct {
		name string
		q    string
		want []string
	}{
		{name: "and example", q: "level:error AND service:api", want: []string{"a1", "a3"}},
		{name: "or", q: "level:error OR level:warn", want: []string{"a1", "w1", "a3", "d1"}},
		{name: "or across fields", q: "level:info OR service:db", want: []string{"a2", "d1", "sv"}},
		{name: "and tighter than or", q: "level:error AND service:api OR service:db", want: []string{"a1", "a3", "d1"}},
		{name: "parens change precedence", q: "level:error AND (service:api OR service:db)", want: []string{"a1", "a3"}},
		{name: "parens or then and", q: "(level:error OR level:warn) AND service:api", want: []string{"a1", "a3"}},
		{name: "parens worker or db", q: "(service:worker OR service:db) AND level:error", want: []string{"w1"}},
		{name: "three term and", q: "level:error AND service:api AND message:timeout", want: []string{"a1"}},
		{name: "default field", q: "timeout", want: []string{"a1", "d1"}},
		{name: "explicit message field", q: "message:timeout", want: []string{"a1", "d1"}},
		{name: "timeout is not every field", q: "service:timeout", want: []string{"sv"}},
		{name: "analyzed query term", q: "level:ERROR AND service:API", want: []string{"a1", "a3"}},
		{name: "punctuation on query term", q: "message:timeout!", want: []string{"a1", "d1"}},
		{name: "lowercase operator", q: "level:error and service:api", want: []string{"a1", "a3"}},
		{name: "extra whitespace", q: "  level:error   AND   service:api  ", want: []string{"a1", "a3"}},
		{name: "missing term", q: "level:debug", want: []string{}},
		{name: "contradiction", q: "level:error AND level:info", want: []string{}},
		{name: "and with missing", q: "message:missing AND service:api", want: []string{}},
		{name: "or after empty and", q: "(level:missing AND service:api) OR service:db", want: []string{"d1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := idx.Search(tt.q, 10)
			if err != nil {
				t.Fatalf("Search(%q): %v", tt.q, err)
			}
			if res.Hits == nil {
				t.Fatal("hits is nil; want an empty list when nothing matches")
			}
			got := hitIDs(res.Hits)
			if !equalStrings(got, tt.want) {
				t.Fatalf("Search(%q) ids = %v, want %v", tt.q, got, tt.want)
			}
			node, err := parseQuery(tt.q)
			if err != nil {
				t.Fatalf("parseQuery(%q): %v", tt.q, err)
			}
			if res.PostingsLookups != node.termCount() {
				t.Fatalf("postings_lookups = %d, want %d term lookups", res.PostingsLookups, node.termCount())
			}
			if res.DocsExamined != len(res.Hits) {
				t.Fatalf("docs_examined = %d, hits = %d", res.DocsExamined, len(res.Hits))
			}
			if tt.q == "level:error AND service:api" {
				for _, h := range res.Hits {
					if h["_id"] == "a1" && h["message"] != "timeout calling db" {
						t.Fatalf("stored message = %q, want original field text", h["message"])
					}
				}
			}
		})
	}
}

func TestPostingsVersusFullScan(t *testing.T) {
	_, idx := newTestIndex(t)
	const nDocs = 150
	const wantHits = 4
	for i := 0; i < nDocs; i++ {
		fields := map[string]string{
			"service": "worker",
			"level":   "info",
			"message": "heartbeat ok",
		}
		switch {
		case i < wantHits:
			fields["service"] = "api"
			fields["level"] = "error"
			fields["message"] = "timeout calling db"
		case i < 30:
			fields["service"] = "api"
			fields["level"] = "info"
			fields["message"] = "request ok"
		case i < 50:
			fields["service"] = "worker"
			fields["level"] = "error"
			fields["message"] = "disk full"
		}
		mustIndex(t, idx, strconv.Itoa(i+1), fields)
	}
	if err := idx.checkPostings(); err != nil {
		t.Fatal(err)
	}

	docs := idx.storedDocs()
	if len(docs) != nDocs {
		t.Fatalf("stored docs = %d, want %d", len(docs), nDocs)
	}

	queries := []string{
		"level:error AND service:api",
		"level:error OR service:worker",
		"message:timeout",
		"level:info",
		"(level:error OR level:info) AND service:api",
		"level:debug",
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			res, err := idx.Search(q, nDocs)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			scanned, examined, err := naiveScan(docs, q)
			if err != nil {
				t.Fatalf("naiveScan: %v", err)
			}
			if examined != nDocs {
				t.Fatalf("naive scan examined %d docs, want the full corpus %d", examined, nDocs)
			}
			if !equalStrings(sortedCopy(hitIDs(res.Hits)), sortedCopy(scanned)) {
				t.Fatalf("postings hits %v, naive hits %v", hitIDs(res.Hits), scanned)
			}
			node, err := parseQuery(q)
			if err != nil {
				t.Fatal(err)
			}
			if res.PostingsLookups != node.termCount() {
				t.Fatalf("postings_lookups = %d, want %d", res.PostingsLookups, node.termCount())
			}
			if res.PostingsLookups >= nDocs {
				t.Fatalf("postings_lookups = %d scaled with doc count %d", res.PostingsLookups, nDocs)
			}
		})
	}

	// The selective query hits a handful of docs. Postings lookups stay at the
	// term count while the scan baseline still reads every stored document.
	const q = "level:error AND service:api"
	res, err := idx.Search(q, nDocs)
	if err != nil {
		t.Fatal(err)
	}
	_, examined, err := naiveScan(docs, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != wantHits {
		t.Fatalf("hits = %d, want %d", len(res.Hits), wantHits)
	}
	if res.PostingsLookups != 2 {
		t.Fatalf("postings_lookups = %d, want 2 term lookups", res.PostingsLookups)
	}
	if res.DocsExamined != wantHits {
		t.Fatalf("docs_examined = %d, want %d fetched hits", res.DocsExamined, wantHits)
	}
	if examined != nDocs {
		t.Fatalf("naive examined = %d, want %d", examined, nDocs)
	}
	if res.DocsExamined >= examined {
		t.Fatalf("postings path examined %d docs, scan examined %d", res.DocsExamined, examined)
	}
	wantIDs := []string{"1", "2", "3", "4"}
	if !equalStrings(hitIDs(res.Hits), wantIDs) {
		t.Fatalf("hits = %v, want %v", hitIDs(res.Hits), wantIDs)
	}
}

func TestReindexReplacesDocument(t *testing.T) {
	mgr, idx := newTestIndex(t)
	first := mustIndex(t, idx, "42", map[string]string{
		"service": "api",
		"level":   "error",
		"message": "timeout calling db",
	})
	if first.Seq != 0 || first.Replaced {
		t.Fatalf("first index = %+v, want seq 0 and a new id", first)
	}
	other := mustIndex(t, idx, "7", map[string]string{
		"service": "api",
		"level":   "error",
		"message": "disk full",
	})
	if other.Seq != 1 {
		t.Fatalf("second seq = %d, want 1", other.Seq)
	}

	second := mustIndex(t, idx, "42", map[string]string{
		"service": "api",
		"level":   "info",
		"message": "request ok",
	})
	if !second.Replaced {
		t.Fatal("expected replacement of _id 42")
	}
	if second.ID != "42" {
		t.Fatalf("id = %q, want 42", second.ID)
	}
	if second.Seq == first.Seq {
		t.Fatal("replacement reused the old docID")
	}
	if second.Seq != 2 {
		t.Fatalf("replacement seq = %d, want 2", second.Seq)
	}

	idx, err := mgr.GetIndex("logs")
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.checkPostings(); err != nil {
		t.Fatal(err)
	}

	res, err := idx.Search("level:error AND service:api", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(res.Hits), []string{"7"}) {
		t.Fatalf("level:error hits = %v, want only the doc that was not replaced", hitIDs(res.Hits))
	}
	res, err = idx.Search("message:timeout", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 0 {
		t.Fatalf("old term message:timeout still matched %v", hitIDs(res.Hits))
	}
	res, err = idx.Search("level:info", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(res.Hits), []string{"42"}) {
		t.Fatalf("level:info hits = %v, want 42", hitIDs(res.Hits))
	}
	if res.Hits[0]["message"] != "request ok" {
		t.Fatalf("stored message = %q, want the replacement text", res.Hits[0]["message"])
	}
}

func TestAutoIDAndLimit(t *testing.T) {
	_, idx := newTestIndex(t)
	held := mustIndex(t, idx, "1", map[string]string{"level": "error", "message": "timeout"})
	if held.ID != "1" || held.Seq != 0 {
		t.Fatalf("explicit id result = %+v", held)
	}
	next := mustIndex(t, idx, "", map[string]string{"level": "error", "message": "timeout"})
	if next.ID != "2" || next.Seq != 1 {
		t.Fatalf("auto id skipped or collided: %+v", next)
	}

	for i := 2; i < 15; i++ {
		res := mustIndex(t, idx, "", map[string]string{"level": "error", "message": "timeout"})
		if res.ID != strconv.Itoa(i+1) {
			t.Fatalf("auto id = %q, want %d", res.ID, i+1)
		}
		if res.Seq != uint32(i) {
			t.Fatalf("seq = %d, want %d", res.Seq, i)
		}
	}

	res, err := idx.Search("level:error", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 10 || res.DocsExamined != 10 || res.PostingsLookups != 1 {
		t.Fatalf("limited search = %+v", res)
	}
	if res.Hits[0]["_id"] != "1" || res.Hits[9]["_id"] != "10" {
		t.Fatalf("limit window = %v", hitIDs(res.Hits))
	}

	none, err := idx.Search("level:error", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(none.Hits) != 0 || none.DocsExamined != 0 || none.PostingsLookups != 1 {
		t.Fatalf("limit 0 = %+v", none)
	}
	if none.Hits == nil {
		t.Fatal("limit 0 returned nil hits")
	}
}

func TestRejectUnsupportedQueries(t *testing.T) {
	_, idx := newTestIndex(t)
	tests := []struct {
		name string
		q    string
		sub  string
	}{
		{name: "unclosed phrase", q: `"timeout db`, sub: "unclosed"},
		{name: "one term phrase", q: `"timeout"`, sub: "at least two"},
		{name: "not", q: "NOT level:error", sub: "NOT"},
		{name: "minus", q: "-level:error", sub: "NOT"},
		{name: "bang", q: "!level:error", sub: "NOT"},
		{name: "field minus", q: "level:-error", sub: "NOT"},
		{name: "empty", q: "   ", sub: "empty"},
		{name: "unbalanced", q: "(level:error", sub: "parenthesis"},
		{name: "adjacent terms", q: "level:error service:api", sub: "unexpected token"},
		{name: "multi token", q: "message:foo-bar", sub: "multiple tokens"},
		{name: "too short", q: "level:e", sub: "minimum length"},
		{name: "bare operator", q: "AND", sub: "unexpected operator"},
		{name: "symbolic and", q: "&&", sub: "not supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := idx.Search(tt.q, 10)
			if !errors.Is(err, ErrBadQuery) {
				t.Fatalf("Search(%q) err = %v, want ErrBadQuery", tt.q, err)
			}
			if tt.sub != "" && !strings.Contains(err.Error(), tt.sub) {
				t.Fatalf("Search(%q) err = %q, want it to mention %q", tt.q, err.Error(), tt.sub)
			}
		})
	}
}

func TestConcurrentIndexAndSearch(t *testing.T) {
	mgr, idx := newTestIndex(t)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := strconv.Itoa(i + 1)
			if _, err := idx.IndexDocument(id, map[string]string{
				"level":   "error",
				"service": "api",
				"message": "timeout calling db",
			}); err != nil {
				t.Errorf("index %s: %v", id, err)
			}
			again, err := mgr.GetIndex("logs")
			if err != nil {
				t.Errorf("get index: %v", err)
				return
			}
			if _, err := again.Search("level:error AND service:api", 10); err != nil {
				t.Errorf("search: %v", err)
			}
		}(i)
	}
	wg.Wait()

	res, err := idx.Search("level:error AND service:api", 32)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 32 {
		t.Fatalf("hits = %d, want 32", len(res.Hits))
	}
	if err := idx.checkPostings(); err != nil {
		t.Fatal(err)
	}
}

func newTestIndex(t *testing.T) (*Manager, *Index) {
	t.Helper()
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	idx, err := mgr.CreateIndex("logs")
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	return mgr, idx
}

func mustIndex(t *testing.T, idx *Index, id string, fields map[string]string) IndexResult {
	t.Helper()
	res, err := idx.IndexDocument(id, fields)
	if err != nil {
		t.Fatalf("IndexDocument(%q): %v", id, err)
	}
	return res
}

func hitIDs(hits []map[string]string) []string {
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h["_id"]
	}
	return ids
}

func sortedCopy(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

func equalU32(got, want []uint32) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// naiveScan re-analyzes every stored document and evaluates the boolean query
// against that token set. It does not read postings.
func naiveScan(docs []map[string]string, q string) (ids []string, examined int, err error) {
	node, err := parseQuery(q)
	if err != nil {
		return nil, 0, err
	}
	for _, doc := range docs {
		examined++
		terms := make(map[string]struct{}, 8)
		for _, term := range FieldTerms(doc) {
			terms[term] = struct{}{}
		}
		if node.matches(terms) {
			ids = append(ids, doc["_id"])
		}
	}
	return ids, examined, nil
}
