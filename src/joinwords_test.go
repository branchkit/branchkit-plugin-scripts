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
		{[]string{"aim"}, "aim"},
		{[]string{"aim", "bus"}, "aim or bus"},
		{[]string{"aim", "bus", "car"}, "aim, bus or car"},
	} {
		if got := joinWords(tc.in); got != tc.want {
			t.Errorf("joinWords(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
