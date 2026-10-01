package index

import "testing"

func TestAnalyze(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "lowercase", in: "ERROR", want: []string{"error"}},
		{name: "mixed case phrase", in: "Timeout Calling DB", want: []string{"timeout", "calling", "db"}},
		{name: "punctuation split", in: "foo...bar, baz!", want: []string{"foo", "bar", "baz"}},
		{name: "underscore splits", in: "foo_bar", want: []string{"foo", "bar"}},
		{name: "min length drops singles", in: "a b cc d", want: []string{"cc"}},
		{name: "single letter", in: "A", want: nil},
		{name: "two letters kept", in: "OK", want: []string{"ok"}},
		{name: "single digit dropped", in: "7", want: nil},
		{name: "two digits kept", in: "42", want: []string{"42"}},
		{name: "alnum kept together", in: "timeout123", want: []string{"timeout123"}},
		{name: "punctuation only", in: " ... !!! ", want: nil},
		{name: "empty", in: "", want: nil},
		{name: "apostrophe splits", in: "don't", want: []string{"don"}},
		{name: "unicode lowercase", in: "Café!", want: []string{"café"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Analyze(tt.in)
			if !equalStrings(got, tt.want) {
				t.Fatalf("Analyze(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestFieldTerms(t *testing.T) {
	got := FieldTerms(map[string]string{
		"_id":     "should-not-index",
		"level":   "ERROR",
		"message": "Timeout, calling DB! x",
		"service": "api",
		"note":    "error error",
	})
	want := []string{
		"level:error",
		"message:calling",
		"message:db",
		"message:timeout",
		"note:error",
		"service:api",
	}
	if !equalStrings(got, want) {
		t.Fatalf("FieldTerms = %#v, want %#v", got, want)
	}
}

func equalStrings(got, want []string) bool {
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
