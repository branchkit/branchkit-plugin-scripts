package main

import "testing"

// The chooser's question is read aloud by speech renderers, so the list of
// words must read like a sentence.
func TestJoinWordsReadsAloud(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"harp"}, "harp"},
		{[]string{"harp", "arch"}, "harp or arch"},
		{[]string{"harp", "arch", "drum"}, "harp, arch or drum"},
	} {
		if got := joinWords(tc.in); got != tc.want {
			t.Errorf("joinWords(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
