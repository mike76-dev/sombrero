package main

import (
	"errors"
	"testing"

	"github.com/mike76-dev/sombrero/stores"
)

// TestRemoveShare covers what unregistering a share finds on the server: a
// share that is loaded, one that is in use, and one that was never loaded at
// all. The last is the ordinary state of a renterd share after a restart —
// nothing puts it in the list until a client asks for it — and refusing to
// remove it would leave it in the database with no way to get rid of it.
func TestRemoveShare(t *testing.T) {
	t.Run("a share that was never loaded is removed", func(t *testing.T) {
		h := newSMBTest(t)

		if err := h.srv.RemoveShare(stores.Share{Name: "never-loaded"}); err != nil {
			t.Fatalf("RemoveShare: want the share unregistered, got %v", err)
		}
	})

	t.Run("a share in use is kept", func(t *testing.T) {
		h := newSMBTest(t)

		h.share.mu.Lock()
		h.share.currentUses = 1
		h.share.mu.Unlock()

		err := h.srv.RemoveShare(stores.Share{Name: h.share.name})
		if !errors.Is(err, stores.ErrShareInUse) {
			t.Fatalf("RemoveShare: want ErrShareInUse, got %v", err)
		}

		h.srv.mu.Lock()
		_, found := h.srv.shareList[h.share.name]
		h.srv.mu.Unlock()
		if !found {
			t.Error("a share that is in use must stay in the list")
		}
	})

	t.Run("a loaded share is taken off the list", func(t *testing.T) {
		h := newSMBTest(t)

		if err := h.srv.RemoveShare(stores.Share{Name: h.share.name}); err != nil {
			t.Fatalf("RemoveShare: %v", err)
		}

		h.srv.mu.Lock()
		_, found := h.srv.shareList[h.share.name]
		h.srv.mu.Unlock()
		if found {
			t.Error("the share is still in the list")
		}
	})
}
