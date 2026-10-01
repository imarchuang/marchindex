package index

import (
	"sort"
	"strings"
	"unicode"
)

// DefaultField is the field searched when a query term has no "field:" prefix.
const DefaultField = "message"

// minTokenLen is the minimum token length kept by Analyze.
const minTokenLen = 2

// Analyze lowercases text, splits on non-alphanumeric characters, and drops
// tokens shorter than minTokenLen.
func Analyze(text string) []string {
	lower := strings.ToLower(text)
	var tokens []string
	var buf []rune
	flush := func() {
		if len(buf) >= minTokenLen {
			tokens = append(tokens, string(buf))
		}
		buf = buf[:0]
	}
	for _, r := range lower {
		if isTokenRune(r) {
			buf = append(buf, r)
		} else {
			flush()
		}
	}
	flush()
	return tokens
}

func isTokenRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// scopedTerm is the inverted-index key for a token in a field ("level:error").
func scopedTerm(field, token string) string {
	return field + ":" + token
}

// FieldTerms returns the unique field-scoped terms for every field except _id.
// The result is sorted.
func FieldTerms(fields map[string]string) []string {
	seen := make(map[string]struct{})
	var out []string
	for field, value := range fields {
		if field == "" || field == "_id" {
			continue
		}
		for _, tok := range Analyze(value) {
			key := scopedTerm(field, tok)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}
