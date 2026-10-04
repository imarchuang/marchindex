package index

import (
	"sort"
	"strings"
)

// fuzzyDocs unions the postings of every term in field within maxDist edits
// of term. Distance 0 is one exact lookup, including when the term is absent.
// The dictionary is scanned in full; keys are field:term strings already in memory.
func fuzzyDocs(postings map[string][]docPosting, field, term string, maxDist int, lookups *int) []uint32 {
	if maxDist == 0 {
		*lookups++
		return append([]uint32(nil), postingIDs(postings[scopedTerm(field, term)])...)
	}
	var acc []uint32
	for _, key := range fuzzyTermKeys(postings, field, term, maxDist) {
		*lookups++
		acc = union(acc, postingIDs(postings[key]))
	}
	return acc
}

// fuzzyTermKeys returns field-scoped dictionary keys within maxDist of term,
// sorted so the union of their postings is stable.
func fuzzyTermKeys(postings map[string][]docPosting, field, term string, maxDist int) []string {
	prefix := scopedTerm(field, "")
	var keys []string
	for key := range postings {
		got, ok := strings.CutPrefix(key, prefix)
		if !ok || got == "" {
			continue
		}
		if withinEditDistance(term, got, maxDist) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// withinEditDistance reports whether Levenshtein(a, b) is at most maxDist.
// Insert, delete, and substitute each cost 1. A transposition costs 2.
func withinEditDistance(a, b string, maxDist int) bool {
	if maxDist < 0 {
		return false
	}
	ar := []rune(a)
	br := []rune(b)
	diff := len(ar) - len(br)
	if diff < 0 {
		diff = -diff
	}
	if diff > maxDist {
		return false
	}
	return levenshtein(ar, br) <= maxDist
}

func levenshtein(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := cur[j-1] + 1
			sub := prev[j-1] + cost
			cur[j] = min(del, ins, sub)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
