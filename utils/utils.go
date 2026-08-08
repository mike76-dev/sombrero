package utils

import (
	"cmp"
	"math"
	"strings"
)

// Roundup calculates the upper bound of a number alighed to the specified byte number.
func Roundup(x, bound int) int {
	return (x + (bound - 1)) &^ (bound - 1)
}

// ExtractFilename extracts the name of the file from a renterd object path.
func ExtractFilename(path string) (filepath string, filename string, isDir bool) {
	if path == "" || path == "/" {
		return "", "", true
	}

	// The separator is taken off if it is there rather than on the strength of a key always
	// opening with one. Counting past the first byte regardless eats a character out of any name
	// that arrives without it, and says nothing while doing so.
	filepath = strings.TrimPrefix(path, "/")
	if path[len(path)-1] == '/' {
		isDir = true
		filepath = filepath[:len(filepath)-1]
	}

	i := strings.LastIndex(filepath, "/")
	if i < 0 {
		filename = filepath
	} else {
		filename = filepath[i+1:]
	}

	return
}

// TrimPath extracts the filename from an OS-specific path.
func TrimPath(path string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	parts := strings.Split(path, "/")
	return parts[len(parts)-1]
}

// TrimName extracts the file path from an OS-specific path.
func TrimName(path string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	parts := strings.Split(path, "/")
	return strings.Join(parts[:len(parts)-1], "/")
}

// FindMinKey finds a key-value pair with the smallest key in the map.
func FindMinKey[T any](m map[uint64]T) (key uint64, value T) {
	// The first pair seen is taken whatever it is, rather than only one that beats the key this
	// starts at. A map holding nothing but the very key at that end would otherwise come back
	// naming it and carrying no value, the comparison never having been true.
	key = math.MaxUint64
	var found bool
	for k, v := range m {
		if !found || k < key {
			key, value, found = k, v, true
		}
	}
	return
}

// FindMaxKey finds a key-value pair with the greatest key in the map.
func FindMaxKey[T any](m map[uint64]T) (key uint64, value T) {
	// As above, and the key at this end is zero, which is the first message ID of a connection.
	var found bool
	for k, v := range m {
		if !found || k > key {
			key, value, found = k, v, true
		}
	}
	return
}

// IsOverlapped returns true if there's at least one match between the two slices.
func IsOverlapped[T comparable](a, b []T) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}

	if len(a) > len(b) {
		a, b = b, a
	}

	set := make(map[T]struct{}, len(a))
	for _, v := range a {
		set[v] = struct{}{}
	}

	for _, v := range b {
		if _, ok := set[v]; ok {
			return true
		}
	}

	return false
}

// Subset returns the largest common subset of both slices.
func Subset[T comparable](a, b []T) []T {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}

	if len(a) > len(b) {
		a, b = b, a
	}

	set := make(map[T]struct{}, len(a))
	for _, v := range a {
		set[v] = struct{}{}
	}

	var c []T
	for _, v := range b {
		if _, ok := set[v]; ok {
			c = append(c, v)
		}
	}

	return c
}

// FirstMatch returns the first occurence in a that is present in b.
func FirstMatch[T comparable](a, b []T) T {
	var c T
	if len(a) == 0 || len(b) == 0 {
		return c
	}

	set := make(map[T]struct{}, len(b))
	for _, v := range b {
		set[v] = struct{}{}
	}

	for _, v := range a {
		if _, ok := set[v]; ok {
			return v
		}
	}

	return c
}

// Equal returns true if two slices are equal, which means they contain
// sets of the same elements, no matter in which order.
func Equal[T comparable](a, b []T) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}

	if len(a) == 0 || len(b) == 0 {
		return false
	}

	if len(a) > len(b) {
		a, b = b, a
	}

	set := make(map[T]struct{}, len(a))
	for _, v := range a {
		set[v] = struct{}{}
	}

	for _, v := range b {
		if _, ok := set[v]; !ok {
			return false
		}
	}

	return true
}

// MaxCommon returns the greatest common element of the two slices.
// Only positive values can be compared this way.
func MaxCommon[T cmp.Ordered](a, b []T) T {
	var c T
	if len(a) == 0 || len(b) == 0 {
		return c
	}

	if len(a) > len(b) {
		a, b = b, a
	}

	set := make(map[T]struct{}, len(a))
	for _, v := range a {
		set[v] = struct{}{}
	}

	for _, v := range b {
		if _, ok := set[v]; ok {
			if v > c {
				c = v
			}
		}
	}

	return c
}
