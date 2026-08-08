package utils

import "testing"

// TestMatchPatternTakesNamesLiterally is the reason this matcher exists at all: a name is
// compared character by character, and the punctuation a shell glob would read as syntax means
// nothing here.
func TestMatchPatternTakesNamesLiterally(t *testing.T) {
	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"[MS-SMB2].pdf", "[MS-SMB2].pdf", true},
		{"[MS-SMB2].pdf", "M.pdf", false},
		{"[MS-SMB2].pdf", "S.pdf", false},
		{"[MS-SMB2].pdf", "MS-SMB2.pdf", false},
		{"*.pdf", "[MS-SMB2].pdf", true},
		{"[MS-*", "[MS-SMB2].pdf", true},

		// A backslash is an escape character to a glob matcher and a plain character here.
		{`a\b.txt`, `a\b.txt`, true},
		{`a\b.txt`, "ab.txt", false},

		// The rest of the punctuation that means something to a glob and nothing to SMB.
		{"{a,b}.txt", "{a,b}.txt", true},
		{"[!x].txt", "[!x].txt", true},
		{"[a-z].txt", "b.txt", false},
	}

	for _, test := range tests {
		if got := MatchPattern(test.pattern, test.name); got != test.want {
			t.Errorf("MatchPattern(%q, %q) = %v, want %v", test.pattern, test.name, got, test.want)
		}
	}
}

// TestMatchPatternWildcards is the two wildcards every client sends.
func TestMatchPatternWildcards(t *testing.T) {
	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"*", "anything.txt", true},
		{"*", "", true},
		{"*.txt", "notes.txt", true},
		{"*.txt", "notes.txt.bak", false},
		{"a*b*c", "abc", true},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "acb", false},
		{"**", "abc", true},

		// A question mark stands for exactly one character, and never for none of them.
		{"?.txt", "a.txt", true},
		{"?.txt", ".txt", false},
		{"a?", "a", false},
		{"a?", "ab", true},
		{"a?", "abc", false},

		// One character means one character of the name, not one byte of it.
		{"?", "é", true},
		{"?", "日", true},

		// The comparison is done as it is stored, so case is part of the name.
		{"notes.txt", "NOTES.TXT", false},

		// Nothing matches nothing, and nothing else.
		{"", "", true},
		{"", "a", false},
	}

	for _, test := range tests {
		if got := MatchPattern(test.pattern, test.name); got != test.want {
			t.Errorf("MatchPattern(%q, %q) = %v, want %v", test.pattern, test.name, got, test.want)
		}
	}
}

// TestMatchPatternDOSWildcards is the three wildcards a Windows client sends in place of the
// ones the user typed: "*.*" goes out as DOS_STAR, DOS_DOT, DOS_STAR, and "*." as DOS_STAR
// followed by DOS_DOT, which is how a client asks for the names that carry no extension.
func TestMatchPatternDOSWildcards(t *testing.T) {
	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		// "*.*" matches everything, extension or not.
		{`<"<`, "notes.txt", true},
		{`<"<`, "archive.tar.gz", true},
		{`<"<`, "README", true},
		{`<"<`, "", true},

		// "*." is the names with nothing after the last period, which means no period at all.
		{`<"`, "README", true},
		{`<"`, "notes.txt", false},

		// DOS_STAR on its own stops at the final period and so cannot reach the end of a name
		// that has an extension.
		{"<", "README", true},
		{"<", "notes.txt", false},
		{"<.txt", "notes.txt", true},
		{"<.txt", "a.b.txt", true},

		// DOS_QM takes one character, or gives up quietly at a period or at the end of the name.
		{">>>>>>>>", "notes.txt", false},
		{">>>>>.txt", "notes.txt", true},
		{">>>>>>>.txt", "notes.txt", true},
		{">>>.txt", "notes.txt", false},
		{">>>>>>>>", "notes", true},
	}

	for _, test := range tests {
		if got := MatchPattern(test.pattern, test.name); got != test.want {
			t.Errorf("MatchPattern(%q, %q) = %v, want %v", test.pattern, test.name, got, test.want)
		}
	}
}

// TestMatchPatternDoesNotBacktrack is the pattern that a matcher written the obvious way spends
// the rest of the afternoon on. The pattern comes off the wire, so a client that sends this one
// must cost the server no more than the length of the pattern times the length of the name.
func TestMatchPatternDoesNotBacktrack(t *testing.T) {
	pattern := "*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*b"
	name := ""
	for range 256 {
		name += "a"
	}

	if MatchPattern(pattern, name) {
		t.Error("a name with no b in it matched a pattern that ends in one")
	}
}
