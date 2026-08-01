package utils

import (
	"math"
	"slices"
	"testing"
)

// TestRoundup is the alignment every structure in a message is laid out on. A value already on
// the boundary stays where it is, which is the case a rounding written the obvious way gets
// wrong by pushing it a whole bound further on.
func TestRoundup(t *testing.T) {
	for _, tt := range []struct {
		x, bound, want int
	}{
		{0, 8, 0},
		{1, 8, 8},
		{7, 8, 8},
		{8, 8, 8},
		{9, 8, 16},
		{0, 1, 0},
		{5, 1, 5},
		{1, 2, 2},
		{2, 2, 2},
		{3, 4, 4},
		{65, 64, 128},
	} {
		if got := Roundup(tt.x, tt.bound); got != tt.want {
			t.Errorf("rounding %d up to a multiple of %d gave %d, want %d", tt.x, tt.bound, got, tt.want)
		}
	}
}

// TestExtractFilename splits an object key the way the store hands it over: the leading separator
// is not part of the path, and a key ending in one names a directory rather than a file.
func TestExtractFilename(t *testing.T) {
	for _, tt := range []struct {
		name             string
		path             string
		wantPath         string
		wantName         string
		wantIsDirectiory bool
	}{
		{"the root", "/", "", "", true},
		{"nothing at all", "", "", "", true},
		{"a file at the root", "/file.txt", "file.txt", "file.txt", false},
		{"a file in a directory", "/dir/file.txt", "dir/file.txt", "file.txt", false},
		{"a file further down", "/a/b/c/file.txt", "a/b/c/file.txt", "file.txt", false},
		{"a directory at the root", "/dir/", "dir", "dir", true},
		{"a directory further down", "/a/b/dir/", "a/b/dir", "dir", true},
		{"a name with a dot in it", "/dir/archive.tar.gz", "dir/archive.tar.gz", "archive.tar.gz", false},
		{"a name with a space in it", "/my documents/a file.txt", "my documents/a file.txt", "a file.txt", false},

		// Keys reach this without a leading separator: a named pipe is stored under a bare name,
		// and only a switch over three of them elsewhere keeps the rest away from here. Counting
		// past the first byte regardless would hand back "amr" for a pipe called "samr", and
		// nothing further along would know the name had been cut.
		{"a bare name", "samr", "samr", "samr", false},
		{"a bare path", "dir/file.txt", "dir/file.txt", "file.txt", false},
		{"a bare directory", "dir/", "dir", "dir", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path, name, isDir := ExtractFilename(tt.path)

			if path != tt.wantPath {
				t.Errorf("the path came out %q, want %q", path, tt.wantPath)
			}
			if name != tt.wantName {
				t.Errorf("the name came out %q, want %q", name, tt.wantName)
			}
			if isDir != tt.wantIsDirectiory {
				t.Errorf("it was called a directory: %v, want %v", isDir, tt.wantIsDirectiory)
			}
		})
	}
}

// TestTrimPathAndTrimName cut a name apart on either separator, since the path may have been
// written on either kind of system. Between them the two halves have to account for the whole of
// what they were given.
func TestTrimPathAndTrimName(t *testing.T) {
	for _, tt := range []struct {
		name     string
		path     string
		wantName string
		wantPath string
	}{
		{"nothing at all", "", "", ""},
		{"a bare name", "file.txt", "file.txt", ""},
		{"separated the one way", "dir/file.txt", "file.txt", "dir"},
		{"separated the other way", "dir\\file.txt", "file.txt", "dir"},
		{"separated both ways at once", "a\\b/c\\file.txt", "file.txt", "a/b/c"},
		{"further down", "a/b/c/file.txt", "file.txt", "a/b/c"},
		{"ending in a separator", "dir/", "", "dir"},
		{"nothing but a separator", "/", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := TrimPath(tt.path); got != tt.wantName {
				t.Errorf("the name came out %q, want %q", got, tt.wantName)
			}
			if got := TrimName(tt.path); got != tt.wantPath {
				t.Errorf("the path came out %q, want %q", got, tt.wantPath)
			}
		})
	}
}

// TestFindMinKeyAndFindMaxKey walk a map for the pair at either end. The request queue is drawn
// down by the smallest message ID and the credit window is read at the largest, so both the key
// and the value that goes with it have to come back.
func TestFindMinKeyAndFindMaxKey(t *testing.T) {
	m := map[uint64]string{
		4: "four",
		1: "one",
		9: "nine",
		2: "two",
	}

	if key, value := FindMinKey(m); key != 1 || value != "one" {
		t.Errorf("the smallest is %d/%q, want 1/%q", key, value, "one")
	}
	if key, value := FindMaxKey(m); key != 9 || value != "nine" {
		t.Errorf("the largest is %d/%q, want 9/%q", key, value, "nine")
	}
}

// TestFindMinKeyAndFindMaxKeyAtTheEndsOfTheRange are the keys sitting on the value each search
// starts from. A search that only takes a pair beating where it started would name that key and
// hand back nothing for it, having never once found the comparison true — and zero is the message
// ID a connection opens with, so it is the first key the credit window ever holds.
func TestFindMinKeyAndFindMaxKeyAtTheEndsOfTheRange(t *testing.T) {
	if key, value := FindMaxKey(map[uint64]string{0: "the first message"}); key != 0 || value != "the first message" {
		t.Errorf("the largest is %d/%q, want 0/%q", key, value, "the first message")
	}

	if key, value := FindMinKey(map[uint64]string{math.MaxUint64: "the last"}); key != math.MaxUint64 || value != "the last" {
		t.Errorf("the smallest is %d/%q, want the last key/%q", key, value, "the last")
	}

	// A single pair is both ends of the map at once.
	single := map[uint64]string{7: "seven"}
	if key, value := FindMinKey(single); key != 7 || value != "seven" {
		t.Errorf("the smallest of one is %d/%q", key, value)
	}
	if key, value := FindMaxKey(single); key != 7 || value != "seven" {
		t.Errorf("the largest of one is %d/%q", key, value)
	}
}

// TestFindMinKeyAndFindMaxKeyOnAnEmptyMap is the search with nothing to find. The callers guard
// against it by looking at the length first, and what comes back is the value each search starts
// from.
func TestFindMinKeyAndFindMaxKeyOnAnEmptyMap(t *testing.T) {
	empty := map[uint64]string{}

	if key, value := FindMinKey(empty); key != math.MaxUint64 || value != "" {
		t.Errorf("searching an empty map gave %d/%q", key, value)
	}
	if key, value := FindMaxKey(empty); key != 0 || value != "" {
		t.Errorf("searching an empty map gave %d/%q", key, value)
	}
}

// TestIsOverlapped asks whether two lists have anything in common, which is how a request naming
// several things is checked against what the server will accept.
func TestIsOverlapped(t *testing.T) {
	for _, tt := range []struct {
		name string
		a, b []int
		want bool
	}{
		{"nothing on either side", nil, nil, false},
		{"nothing on one side", nil, []int{1, 2}, false},
		{"nothing on the other", []int{1, 2}, nil, false},
		{"nothing in common", []int{1, 2}, []int{3, 4}, false},
		{"one in common", []int{1, 2}, []int{2, 3}, true},
		{"all in common", []int{1, 2}, []int{1, 2}, true},
		{"a short list inside a long one", []int{3}, []int{1, 2, 3, 4, 5}, true},
		{"a long list around a short one", []int{1, 2, 3, 4, 5}, []int{3}, true},
		{"the same thing twice on one side", []int{1, 1}, []int{1}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// The question is symmetric, and the search swaps the lists by length, so both
			// orders have to give the same answer.
			if got := IsOverlapped(tt.a, tt.b); got != tt.want {
				t.Errorf("asked one way it said %v, want %v", got, tt.want)
			}
			if got := IsOverlapped(tt.b, tt.a); got != tt.want {
				t.Errorf("asked the other way it said %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSubset keeps what the two lists have in common. What comes back is drawn from the longer
// list, so the order is that of whichever was longer rather than of the argument it was passed as.
func TestSubset(t *testing.T) {
	for _, tt := range []struct {
		name string
		a, b []int
		want []int
	}{
		{"nothing on either side", nil, nil, nil},
		{"nothing on one side", nil, []int{1, 2}, nil},
		{"nothing in common", []int{1, 2}, []int{3, 4}, nil},
		{"some in common", []int{1, 2, 3}, []int{2, 3, 4}, []int{2, 3}},
		{"all in common", []int{1, 2}, []int{1, 2}, []int{1, 2}},
		{"one inside the other", []int{2}, []int{1, 2, 3}, []int{2}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Subset(tt.a, tt.b); !slices.Equal(got, tt.want) {
				t.Errorf("what they have in common came out %v, want %v", got, tt.want)
			}
		})
	}
}

// TestFirstMatch takes the first thing in the first list that the second also holds. Order is the
// whole point: the list is in the order of preference of whoever sent it.
func TestFirstMatch(t *testing.T) {
	for _, tt := range []struct {
		name string
		a, b []string
		want string
	}{
		{"nothing on either side", nil, nil, ""},
		{"nothing on one side", nil, []string{"a"}, ""},
		{"nothing on the other", []string{"a"}, nil, ""},
		{"nothing in common", []string{"a", "b"}, []string{"c"}, ""},
		{"the first of them", []string{"a", "b"}, []string{"a", "b"}, "a"},
		{"further along", []string{"a", "b", "c"}, []string{"c", "b"}, "b"},
		{"the order of the first list settles it", []string{"b", "a"}, []string{"a", "b"}, "b"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := FirstMatch(tt.a, tt.b); got != tt.want {
				t.Errorf("the first match came out %q, want %q", got, tt.want)
			}
		})
	}
}

// TestEqual asks whether two lists hold the same things, the order being nothing to do with it.
// It stands behind the check that a client asked the second time for what it asked the first,
// which is what keeps a negotiation from being talked down to a weaker dialect on the way.
func TestEqual(t *testing.T) {
	for _, tt := range []struct {
		name string
		a, b []int
		want bool
	}{
		{"nothing on either side", nil, nil, true},
		{"nothing against an empty list", nil, []int{}, true},
		{"nothing against something", nil, []int{1}, false},
		{"the same in the same order", []int{1, 2, 3}, []int{1, 2, 3}, true},
		{"the same in another order", []int{3, 1, 2}, []int{1, 2, 3}, true},
		{"one of them different", []int{1, 2, 3}, []int{1, 2, 4}, false},
		{"one of them missing", []int{1, 2}, []int{1, 2, 3}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Equal(tt.a, tt.b); got != tt.want {
				t.Errorf("asked one way it said %v, want %v", got, tt.want)
			}
			if got := Equal(tt.b, tt.a); got != tt.want {
				t.Errorf("asked the other way it said %v, want %v", got, tt.want)
			}
		})
	}
}

// TestEqualCountsThingsAsASet is what the name says in as many words: it is the set of elements
// that is compared, so a list holding the same thing twice is the same as one holding it once.
// The check on a re-negotiated dialect list leans on this, and it is worth writing down that a
// client may repeat a dialect and still be found to have asked for what it asked before.
func TestEqualCountsThingsAsASet(t *testing.T) {
	if !Equal([]int{1, 1, 2}, []int{1, 2}) {
		t.Error("a list holding the same thing twice was not the set it stands for")
	}
}

// TestMaxCommon takes the greatest thing the two lists have in common, which is how the dialect a
// connection settles on is picked out of what each side offers.
func TestMaxCommon(t *testing.T) {
	for _, tt := range []struct {
		name string
		a, b []uint16
		want uint16
	}{
		{"nothing on either side", nil, nil, 0},
		{"nothing on one side", nil, []uint16{0x0202}, 0},
		{"nothing in common", []uint16{0x0202}, []uint16{0x0311}, 0},
		{"one in common", []uint16{0x0202, 0x0210}, []uint16{0x0210, 0x0300}, 0x0210},
		{"the greatest of several", []uint16{0x0202, 0x0210, 0x0300, 0x0311}, []uint16{0x0202, 0x0300}, 0x0300},
		{"everything on both sides", []uint16{0x0202, 0x0311}, []uint16{0x0202, 0x0311}, 0x0311},
		{"the order it is offered in is nothing to do with it", []uint16{0x0311, 0x0202}, []uint16{0x0202, 0x0311}, 0x0311},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := MaxCommon(tt.a, tt.b); got != tt.want {
				t.Errorf("the greatest in common came out %#04x, want %#04x", got, tt.want)
			}
			if got := MaxCommon(tt.b, tt.a); got != tt.want {
				t.Errorf("asked the other way it came out %#04x, want %#04x", got, tt.want)
			}
		})
	}
}

// TestMaxCommonSaysNothingWithAZero is the limit the comment on it draws, written down so that a
// caller reading the tests finds it. The search starts at the zero value and nothing below that
// can be picked out, so lists whose only common element is zero, or none, both come back zero.
// Every caller here compares dialects, and no dialect is zero.
func TestMaxCommonSaysNothingWithAZero(t *testing.T) {
	if got := MaxCommon([]int{0, 5}, []int{0, 7}); got != 0 {
		t.Errorf("the greatest in common came out %d, want 0", got)
	}
	if got := MaxCommon([]int{-5, -1}, []int{-5, -2}); got != 0 {
		t.Errorf("a list of negative numbers came out %d, want 0", got)
	}
}
