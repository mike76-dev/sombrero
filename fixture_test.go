package main

import (
	"reflect"
	"testing"
	"time"
)

// TestFixtureHasEveryTable walks the objects the harness stands up and fails on any map or
// channel left nil, and on any duration left at zero.
//
// The fixture used to build these by hand, which meant a table added to the server reached the
// tests only if somebody remembered to add it here too. Three times it was not: the lease table
// list went missing and every leased create panicked, and twice a field of the connection was
// left at its zero value and the requests under test were refused before they reached the code
// they were written for. The fixture now goes through the same constructors as the server, and
// this is what says so: it fails on the omission itself rather than on whatever the omission
// happens to break first.
//
// Durations are checked for the same reason and were added after the same thing happened again:
// the acknowledgment timers became fields of the server, and a fixture that missed them ran
// every break on a zero timer, ending it the instant it was sent. A zero duration is not a
// harmless default here - it is the most aggressive value there is.
func TestFixtureHasEveryTable(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	for _, obj := range []struct {
		name string
		v    any
	}{
		{"server", h.srv},
		{"connection", cl.conn},
		{"session", cl.ss},
		{"treeConnect", cl.tc},
	} {
		checkFixture(t, obj.name, obj.v)
	}
}

// TestCachingFixtureIsComplete holds the fixture of the granting and breaking unit tests to the
// same standard. It is a second way of building a server, so it is a second chance to miss a
// field, which is exactly what happened to the acknowledgment timers.
func TestCachingFixtureIsComplete(t *testing.T) {
	checkFixture(t, "server", newCachingServer())
}

func checkFixture(t *testing.T, name string, v any) {
	t.Helper()

	val := reflect.ValueOf(v).Elem()
	typ := val.Type()
	durationType := reflect.TypeOf(time.Duration(0))

	for i := range typ.NumField() {
		field := val.Field(i)
		switch {
		case field.Kind() == reflect.Map, field.Kind() == reflect.Chan:
			if field.IsNil() {
				t.Errorf("%s.%s is nil in the fixture: it is created for the running server but not for the tests",
					name, typ.Field(i).Name)
			}
		case field.Type() == durationType:
			if field.Int() == 0 {
				t.Errorf("%s.%s is zero in the fixture: it is set for the running server but not for the tests",
					name, typ.Field(i).Name)
			}
		}
	}
}
