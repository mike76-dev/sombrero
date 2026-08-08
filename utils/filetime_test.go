package utils

import (
	"testing"
	"time"
)

// TestUnixToFiletimeAgainstKnownTimes measures the conversion against dates whose Filetime is
// settled elsewhere. Every timestamp the server reports on a file goes through here, and a client
// shows what it is given: a conversion that is out by the offset between the two epochs is out by
// three hundred and sixty-nine years, which is wrong in a way nothing downstream would catch.
func TestUnixToFiletimeAgainstKnownTimes(t *testing.T) {
	for _, tt := range []struct {
		name string
		unix int64
		want uint64
	}{
		// The Filetime epoch itself: the count of hundred-nanosecond intervals is zero there,
		// and the Unix time is the offset between the two epochs counted backwards.
		{"the epoch Filetime counts from", -11644473600, 0},

		// The Unix epoch, which is the offset expressed in hundred-nanosecond intervals.
		{"the epoch Unix counts from", 0, 116444736000000000},

		{"one second after the Unix epoch", 1, 116444736010000000},
		{"the start of 2001", 978307200, 126227808000000000},
		{"the start of 2024", 1704067200, 133485408000000000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := UnixToFiletime(time.Unix(tt.unix, 0)); got != tt.want {
				t.Errorf("the time came out %d, want %d", got, tt.want)
			}
		})
	}
}

// TestFiletimeToUnixAgainstKnownTimes is the same table read the other way.
func TestFiletimeToUnixAgainstKnownTimes(t *testing.T) {
	for _, tt := range []struct {
		name string
		ft   uint64
		want int64
	}{
		{"the epoch Filetime counts from", 0, -11644473600},
		{"the epoch Unix counts from", 116444736000000000, 0},
		{"the start of 2024", 133485408000000000, 1704067200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := FiletimeToUnix(tt.ft).Unix(); got != tt.want {
				t.Errorf("the time came out %d, want %d", got, tt.want)
			}
		})
	}
}

// TestFiletimeRoundTrip is a time out to a client and back on the request that sets it again.
// Only whole seconds survive, which is what both halves of the conversion count in.
func TestFiletimeRoundTrip(t *testing.T) {
	for _, unix := range []int64{0, 1, 978307200, 1704067200, 4102444800} {
		want := time.Unix(unix, 0)
		if got := FiletimeToUnix(UnixToFiletime(want)); !got.Equal(want) {
			t.Errorf("%v came back as %v", want, got)
		}
	}
}

// TestUnixToFiletimeReportsNothingForATimeNobodySet is the zero time.Time, which is what a field
// nobody filled in holds. It stands for a hundred and thirty centuries before the epoch Filetime
// counts from, so there is no Filetime for it; counted out as an unsigned number it came back as
// a date some fifty thousand years from now, which is what the client would then show. Filetime
// has a way of saying a time is not being reported, and it is the value used here.
func TestUnixToFiletimeReportsNothingForATimeNobodySet(t *testing.T) {
	if got := UnixToFiletime(time.Time{}); got != 0 {
		t.Errorf("a time nobody set came out as %d, want it reported as unset", got)
	}
}

// TestUnixToFiletimeReportsNothingForATimeItCannotTell walks the edge of what Filetime can carry.
// Anything at or before its epoch has no count, and the answer must not be an enormous one.
func TestUnixToFiletimeReportsNothingForATimeItCannotTell(t *testing.T) {
	for _, tt := range []struct {
		name string
		t    time.Time
	}{
		{"a time nobody set", time.Time{}},
		{"the epoch itself", time.Unix(-11644473600, 0)},
		{"a second before the epoch", time.Unix(-11644473601, 0)},
		{"the year 1600", time.Date(1600, time.January, 1, 0, 0, 0, 0, time.UTC)},
		{"as far back as time goes", time.Unix(-1<<62, 0)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := UnixToFiletime(tt.t); got != 0 {
				t.Errorf("a time before the epoch came out as %d, want it reported as unset", got)
			}
		})
	}
}

// TestUnixToFiletimeCarriesTimesTheProtocolActuallySees is the first second Filetime can tell and
// the ordinary dates around it, which must not be swept up by the guard on the epoch.
func TestUnixToFiletimeCarriesTimesTheProtocolActuallySees(t *testing.T) {
	if got := UnixToFiletime(time.Unix(-11644473599, 0)); got != 1e7 {
		t.Errorf("the first second after the epoch came out as %d, want %d", got, uint64(1e7))
	}

	now := time.Now()
	if got := UnixToFiletime(now); got == 0 {
		t.Error("the time now was reported as unset")
	}
}

// TestFiletimeKeepsTheOrderOfTheTimesItTells is what a client sorting by date leans on: a later
// time has to come out a larger count, all the way across the range the protocol carries.
func TestFiletimeKeepsTheOrderOfTheTimesItTells(t *testing.T) {
	times := []time.Time{
		time.Unix(-11644473599, 0),
		time.Unix(0, 0),
		time.Unix(978307200, 0),
		time.Unix(1704067200, 0),
		time.Unix(4102444800, 0),
	}

	for i := 1; i < len(times); i++ {
		before, after := UnixToFiletime(times[i-1]), UnixToFiletime(times[i])
		if before >= after {
			t.Errorf("%v came out %d and the later %v came out %d", times[i-1], before, times[i], after)
		}
	}
}
