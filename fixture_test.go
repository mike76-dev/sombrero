package main

import (
	"reflect"
	"testing"
)

// TestFixtureHasEveryTable walks the objects the harness stands up and fails on any map or
// channel left nil.
//
// The fixture used to build these by hand, which meant a table added to the server reached the
// tests only if somebody remembered to add it here too. Three times it was not: the lease table
// list went missing and every leased create panicked, and twice a field of the connection was
// left at its zero value and the requests under test were refused before they reached the code
// they were written for. The fixture now goes through the same constructors as the server, and
// this is what says so: it fails on the omission itself rather than on whatever the omission
// happens to break first.
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
		val := reflect.ValueOf(obj.v).Elem()
		typ := val.Type()

		for i := range typ.NumField() {
			field := val.Field(i)
			switch field.Kind() {
			case reflect.Map, reflect.Chan:
				if field.IsNil() {
					t.Errorf("%s.%s is nil in the fixture: it is created for the running server but not for the tests",
						obj.name, typ.Field(i).Name)
				}
			}
		}
	}
}
