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

// placed is one token and its position in the field. Position increments for
// every alphanumeric run, including runs shorter than minTokenLen.
type placed struct {
	text string
	pos  uint32
}

// Analyze lowercases text, splits on non-alphanumeric characters, and drops
// tokens shorter than minTokenLen.
func Analyze(text string) []string {
	var tokens []string
	for _, tok := range analyzePositions(text) {
		if len([]rune(tok.text)) >= minTokenLen {
			tokens = append(tokens, tok.text)
		}
	}
	return tokens
}

func analyzePositions(text string) []placed {
	lower := strings.ToLower(text)
	var out []placed
	var buf []rune
	var pos uint32
	flush := func() {
		if len(buf) > 0 {
			out = append(out, placed{text: string(buf), pos: pos})
			pos++
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
	return out
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

// FieldPositions returns the sorted field-scoped terms and every position of
// each term. A repeated term keeps every position. Tokens shorter than
// minTokenLen are omitted here but still consume a position.
func FieldPositions(fields map[string]string) (keys []string, pos map[string][]uint32) {
	pos = make(map[string][]uint32)
	for field, value := range fields {
		if field == "" || field == "_id" {
			continue
		}
		for _, tok := range analyzePositions(value) {
			if len([]rune(tok.text)) < minTokenLen {
				continue
			}
			key := scopedTerm(field, tok.text)
			pos[key] = append(pos[key], tok.pos)
		}
	}
	keys = make([]string, 0, len(pos))
	for key := range pos {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, pos
}
