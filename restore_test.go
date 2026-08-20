package main

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/mike76-dev/sombrero/stores"
)

// indexdShareStore is the store of the tests with one indexd share added to
// what it lists, which the JSON store it wraps will not hold: Lite mode has no
// indexd shares, and the restore is about the ones a database holds.
type indexdShareStore struct {
	stores.Store
}

func (indexdShareStore) GetAllShares() ([]stores.Share, error) {
	return []stores.Share{{
		Name:       "shared-indexd",
		Type:       "indexd",
		ServerName: "127.0.0.1:0",
		DataShards: 1,
	}}, nil
}

// TestRestoringConnectionsSaysItOnce is the outage that lasts. The connections
// of a share carry the work nobody's tree connect triggers - what is buffered
// still has to go up, and the slabs still have to be watched - so a connection
// that cannot be made is tried again for as long as the server runs. What must
// not repeat with it is the complaint.
func TestRestoringConnectionsSaysItOnce(t *testing.T) {
	h := newSMBTest(t)

	// An indexd share the store cannot answer for, which is the failure this is
	// about: the connections of one live in the database, and the store of the
	// tests is not one.
	h.srv.store = indexdShareStore{h.srv.store}

	var out bytes.Buffer
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	reported := make(map[connKey]struct{})

	if h.srv.restoreConnectionsOnce(reported) {
		t.Error("a share whose connections could not be restored was reported as done")
	}
	first := strings.Count(out.String(), "shared-indexd")
	if first == 0 {
		t.Fatal("the failure went unreported")
	}

	// The retry finds the same thing and leaves the log alone.
	if h.srv.restoreConnectionsOnce(reported) {
		t.Error("the second attempt reported the share as done")
	}
	if again := strings.Count(out.String(), "shared-indexd"); again != first {
		t.Errorf("the retry reported the same failure again: %s", strings.TrimSpace(out.String()))
	}
}

// TestRestoringConnectionsSkipsLiteMode is the mode that has none: a renterd
// share is served by the one client the share itself holds, so there is nothing
// to restore and nothing to keep trying for.
func TestRestoringConnectionsSkipsLiteMode(t *testing.T) {
	h := newSMBTest(t)

	// The store of the tests is the JSON one, which is what Lite mode runs on.
	// Returning is the whole of the behaviour: a restore that did not would
	// still be running when the test ended.
	done := make(chan struct{})
	go func() {
		h.srv.restoreConnections()
		close(done)
	}()

	<-done
}
