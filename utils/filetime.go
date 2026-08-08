package utils

import (
	"time"
)

// Unix time is represented in nanoseconds since January 1, 1970.
// Filetime is represented in 100-nanosecond intervals since January 1, 1601.
const filetimeOffset = 11644473600

// UnixToFiletime converts the Unix time to Filetime.
func UnixToFiletime(t time.Time) uint64 {
	// A time before the one Filetime counts from cannot be told in Filetime at all, and the zero
	// time.Time — a timestamp nobody ever set — is the usual way of arriving here with one.
	// Filetime has its own way of saying that, which [MS-FSCC] reads as a time not being reported.
	// Counted out as it stands the subtraction goes below zero and comes back as an enormous
	// count, which a client shows as a date some tens of thousands of years from now.
	secs := t.Unix() + filetimeOffset
	if secs <= 0 {
		return 0
	}

	return uint64(secs) * 1e7
}

// FiletimeToUnix converts Filetime to the Unix time.
func FiletimeToUnix(ft uint64) time.Time {
	return time.Unix(int64(ft)/1e7-filetimeOffset, 0)
}
