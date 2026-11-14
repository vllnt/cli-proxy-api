package util

import "testing"

func TestMatchWildcard(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		value   string
		want    bool
	}{
		{"empty pattern never matches", "", "claude-fable-5", false},
		{"exact", "claude-fable-5", "claude-fable-5", true},
		{"exact mismatch", "claude-fable-5", "claude-fable-6", false},
		{"star matches everything", "*", "anything", true},
		{"star matches empty value", "*", "", true},
		{"prefix star", "claude-fable-*", "claude-fable-5-20260101", true},
		{"prefix star mismatch", "claude-fable-*", "claude-sonnet-5", false},
		{"suffix star", "*-preview", "gpt-6-preview", true},
		{"suffix star mismatch", "*-preview", "gpt-6", false},
		{"middle star", "claude-*-5", "claude-fable-5", true},
		{"middle star mismatch", "claude-*-5", "claude-fable-6", false},
		{"two stars", "*fable*", "claude-fable-5", true},
		{"two stars mismatch", "*fable*", "claude-sonnet-5", false},
		{"ordered segments", "a*b*c", "a-x-b-y-c", true},
		{"out of order segments", "a*c*b", "a-x-b-y-c", false},
		{"case insensitive pattern", "Claude-FABLE-*", "claude-fable-5", true},
		{"case insensitive value", "claude-fable-*", "CLAUDE-Fable-5", true},
		{"overlapping prefix and suffix", "ab*ba", "aba", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MatchWildcard(tt.pattern, tt.value); got != tt.want {
				t.Fatalf("MatchWildcard(%q, %q) = %v, want %v", tt.pattern, tt.value, got, tt.want)
			}
		})
	}
}
