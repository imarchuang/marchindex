package index

import (
	"errors"
	"strings"
	"testing"
)

func TestLevenshtein(t *testing.T) {
	tests := []struct {
		a, b string
		dist int
	}{
		{a: "timeot", b: "timeout", dist: 1},
		{a: "timeot", b: "timeots", dist: 1},
		{a: "timeot", b: "timeot", dist: 0},
		{a: "form", b: "from", dist: 2},
		{a: "eror", b: "error", dist: 1},
		{a: "kitten", b: "sitting", dist: 3},
		{a: "", b: "ab", dist: 2},
	}
	for _, tt := range tests {
		if got := levenshtein([]rune(tt.a), []rune(tt.b)); got != tt.dist {
			t.Fatalf("levenshtein(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.dist)
		}
		if !withinEditDistance(tt.a, tt.b, tt.dist) {
			t.Fatalf("withinEditDistance(%q, %q, %d) = false", tt.a, tt.b, tt.dist)
		}
		if tt.dist > 0 && withinEditDistance(tt.a, tt.b, tt.dist-1) {
			t.Fatalf("withinEditDistance(%q, %q, %d) = true", tt.a, tt.b, tt.dist-1)
		}
	}
}

func TestFuzzySearch(t *testing.T) {
	_, idx := newTestIndex(t)
	mustIndex(t, idx, "near", map[string]string{"service": "api", "level": "error", "message": "timeout calling db"})
	mustIndex(t, idx, "also", map[string]string{"service": "api", "level": "info", "message": "timeots in the log"})
	mustIndex(t, idx, "other", map[string]string{"service": "worker", "level": "error", "message": "disk full"})
	mustIndex(t, idx, "swap", map[string]string{"service": "api", "level": "info", "message": "from the cache"})
	mustIndex(t, idx, "exactish", map[string]string{"service": "api", "message": "timeot once"})

	res, err := idx.Search("timeot~1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(res.Hits), []string{"near", "also", "exactish"}) {
		t.Fatalf("timeot~1 hits = %v", hitIDs(res.Hits))
	}
	// timeout, timeots, and timeot are three dictionary terms.
	if res.PostingsLookups != 3 {
		t.Fatalf("postings_lookups = %d, want 3 expanded terms", res.PostingsLookups)
	}

	exact, err := idx.Search("timeout", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(exact.Hits), []string{"near"}) {
		t.Fatalf("exact timeout hits = %v, want only near", hitIDs(exact.Hits))
	}

	zero, err := idx.Search("timeout~0", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(zero.Hits), hitIDs(exact.Hits)) || zero.PostingsLookups != exact.PostingsLookups {
		t.Fatalf("~0 = %+v, exact lookups %d", zero, exact.PostingsLookups)
	}

	miss, err := idx.Search("zzzz~1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(miss.Hits) != 0 || miss.PostingsLookups != 0 || miss.Hits == nil {
		t.Fatalf("no expansion = %+v", miss)
	}

	scoped, err := idx.Search("level:eror~1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(scoped.Hits), []string{"near", "other"}) {
		t.Fatalf("level:eror~1 hits = %v", hitIDs(scoped.Hits))
	}
	msg, err := idx.Search("message:eror~1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Hits) != 0 {
		t.Fatalf("message:eror~1 hits = %v, want none", hitIDs(msg.Hits))
	}

	// A transposition is two substitutions.
	one, err := idx.Search("form~1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Hits) != 0 {
		t.Fatalf("form~1 hits = %v, want none", hitIDs(one.Hits))
	}
	two, err := idx.Search("form~2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(two.Hits), []string{"swap"}) {
		t.Fatalf("form~2 hits = %v", hitIDs(two.Hits))
	}

	both, err := idx.Search("Timeot~1 AND service:api", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(both.Hits), []string{"near", "also", "exactish"}) {
		t.Fatalf("fuzzy AND hits = %v", hitIDs(both.Hits))
	}

	scanned, _, err := naiveScan(idx.storedDocs(), "timeot~1 AND service:api")
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(sortedCopy(hitIDs(both.Hits)), sortedCopy(scanned)) {
		t.Fatalf("postings %v, naive %v", hitIDs(both.Hits), scanned)
	}

	bad := []string{"timeot~3", "timeot~", "timeot~1~2", "~1", "timeout~a"}
	for _, q := range bad {
		_, err := idx.Search(q, 10)
		if !errors.Is(err, ErrBadQuery) || !strings.Contains(err.Error(), "fuzzy distance") {
			t.Fatalf("Search(%q) err = %v", q, err)
		}
	}
}

func TestFuzzyOnSegmentAndScore(t *testing.T) {
	_, idx := newTestIndex(t)
	mustIndex(t, idx, "low", map[string]string{"message": "timeout once"})
	if _, err := idx.Flush(); err != nil {
		t.Fatal(err)
	}
	mustIndex(t, idx, "high", map[string]string{"message": "timeout timeout timeots"})
	if _, err := idx.Flush(); err != nil {
		t.Fatal(err)
	}
	mustIndex(t, idx, "ram", map[string]string{"message": "timeot"})

	plain, err := idx.Search("timeot~1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(plain.Hits), []string{"low", "high", "ram"}) {
		t.Fatalf("doc order = %v", hitIDs(plain.Hits))
	}
	// seg low: timeout. seg high: timeout + timeots. RAM: timeot. 1+2+1.
	if plain.PostingsLookups != 4 {
		t.Fatalf("postings_lookups = %d, want 4", plain.PostingsLookups)
	}
	if _, ok := plain.Hits[0]["_score"]; ok {
		t.Fatal("default search should not add _score")
	}

	ranked, err := idx.SearchTF("timeot~1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hitIDs(ranked.Hits), []string{"high", "low", "ram"}) {
		t.Fatalf("tf order = %v", hitIDs(ranked.Hits))
	}
	// high sums timeout (2) and timeots (1).
	if ranked.Hits[0]["_score"] != "3" || ranked.Hits[1]["_score"] != "1" || ranked.Hits[2]["_score"] != "1" {
		t.Fatalf("scores = %q %q %q", ranked.Hits[0]["_score"], ranked.Hits[1]["_score"], ranked.Hits[2]["_score"])
	}
}
