package main

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
	"github.com/mike76-dev/sombrero/stores"
)

// The access rights held in the store name their account by ID. Nothing guarantees the account
// is still there by the time a share loads them: the row is meant to go with the account, but a
// share that refuses to come up because of one row it cannot resolve is a far worse failure than
// a principal that ends up without access.

// accountID returns the stored ID of the named account of the test workgroup.
func (h *smbTest) accountID(user string) int {
	h.t.Helper()
	acc, err := h.srv.store.FindAccount(user, h.workgroup)
	if err != nil {
		h.t.Fatalf("could not find the account of %s: %v", user, err)
	}
	return acc.ID
}

// fullRights is an access rights row granting everything to the given account.
func fullRights(id int) stores.AccessRights {
	return stores.AccessRights{
		ShareName:     "files",
		AccountID:     id,
		ReadAccess:    true,
		WriteAccess:   true,
		DeleteAccess:  true,
		ExecuteAccess: true,
	}
}

// canConnect reports whether the share's security maps let the named user in.
func (h *smbTest) canConnect(user string) bool {
	h.t.Helper()
	h.share.mu.Lock()
	defer h.share.mu.Unlock()
	_, ok := h.share.connectSecurity[h.workgroup+"/"+user]
	return ok
}

// The positive control: rights that do name a live account are loaded. Without it the tests
// below would be satisfied by a loader that quietly dropped everything.
func TestAccessRightsAreLoaded(t *testing.T) {
	h := newSMBTest(t)
	h.share.connectSecurity = make(map[string]struct{})
	h.share.fileSecurity = make(map[string]uint32)

	if err := h.srv.loadAccessRights(h.share, []stores.AccessRights{fullRights(h.accountID("alice"))}); err != nil {
		t.Fatalf("loading the access rights failed: %v", err)
	}

	if !h.canConnect("alice") {
		t.Error("alice was granted no access by rights that name her")
	}
}

// One row naming an account that is gone must not cost everybody else their access.
func TestAccessRightsSurviveADanglingRow(t *testing.T) {
	h := newSMBTest(t)
	h.share.connectSecurity = make(map[string]struct{})
	h.share.fileSecurity = make(map[string]uint32)

	// The dangling row comes first, so that a loader which gives up on the first miss is
	// caught rather than happening to have done bob already.
	rights := []stores.AccessRights{
		fullRights(999999),
		fullRights(h.accountID("bob")),
	}

	if err := h.srv.loadAccessRights(h.share, rights); err != nil {
		t.Fatalf("one dangling row failed the whole load: %v", err)
	}

	if !h.canConnect("bob") {
		t.Error("bob lost his access because of a row that named somebody else")
	}
}

// The skipped row must not turn into a grant of its own. The account has no name and no
// workgroup to key the maps by, so a loader that used its zero value would insert "/" - an
// entry that an anonymous session would match.
func TestDanglingAccessRightsGrantNothing(t *testing.T) {
	h := newSMBTest(t)
	h.share.connectSecurity = make(map[string]struct{})
	h.share.fileSecurity = make(map[string]uint32)

	if err := h.srv.loadAccessRights(h.share, []stores.AccessRights{fullRights(999999)}); err != nil {
		t.Fatalf("loading the access rights failed: %v", err)
	}

	h.share.mu.Lock()
	defer h.share.mu.Unlock()
	if len(h.share.connectSecurity) != 0 || len(h.share.fileSecurity) != 0 {
		t.Errorf("a row naming no account granted %v / %v", h.share.connectSecurity, h.share.fileSecurity)
	}
}

// UpdateAccessRights is the same question one row at a time: there is nobody to grant to, and
// the caller should not be failed over it.
func TestUpdateAccessRightsIgnoresAMissingAccount(t *testing.T) {
	h := newSMBTest(t)
	h.share.connectSecurity = make(map[string]struct{})
	h.share.fileSecurity = make(map[string]uint32)

	ss := stores.Share{Name: h.share.name, Type: "renterd"}
	if err := h.srv.UpdateAccessRights(ss, fullRights(999999)); err != nil {
		t.Fatalf("updating the rights of an account that is gone failed: %v", err)
	}

	h.share.mu.Lock()
	defer h.share.mu.Unlock()
	if len(h.share.connectSecurity) != 0 || len(h.share.fileSecurity) != 0 {
		t.Errorf("a row naming no account granted %v / %v", h.share.connectSecurity, h.share.fileSecurity)
	}
}

func TestUpdateAccessRightsGrantsALiveAccount(t *testing.T) {
	h := newSMBTest(t)
	h.share.connectSecurity = make(map[string]struct{})
	h.share.fileSecurity = make(map[string]uint32)

	ss := stores.Share{Name: h.share.name, Type: "renterd"}
	if err := h.srv.UpdateAccessRights(ss, fullRights(h.accountID("alice"))); err != nil {
		t.Fatalf("updating alice's rights failed: %v", err)
	}

	if !h.canConnect("alice") {
		t.Error("alice was granted no access by rights that name her")
	}
}

// The security maps of a share are rewritten by the API - an operator granting or revoking access -
// while connections are reading them to answer creates and tree connects. The two run on different
// goroutines, and a map read against a map write is not a stale answer but a crash.

// TestShareSecurityIsReadSafelyWhileItChanges races the read paths against the API that rewrites
// them. It is a race detector test: what it asserts is that nothing was reported.
func TestShareSecurityIsReadSafelyWhileItChanges(t *testing.T) {
	h := newSMBTest(t)
	h.share.connectSecurity = make(map[string]struct{})
	h.share.fileSecurity = make(map[string]uint32)

	cl := h.dial("alice")
	id := h.accountID("alice")

	// A create for grantAccess to weigh, of the kind a client sends.
	req := request(t, createRequest(1, cl.ss.sessionID, cl.tc.treeID, "file", smb2.OPLOCK_LEVEL_NONE,
		smb2.FILE_OPEN, writeAccess, nil))
	cr := smb2.CreateRequest{Request: *req}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := range 300 {
			rights := fullRights(id)
			if i%2 == 0 { // Revoked, then granted again.
				rights = stores.AccessRights{ShareName: "files", AccountID: id}
			}
			if err := h.srv.UpdateAccessRights(stores.Share{Name: h.share.name}, rights); err != nil {
				t.Errorf("updating the access rights failed: %v", err)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for range 300 {
			grantAccess(cr, cl.tc, cl.ss)
		}
	}()

	wg.Wait()
}

// TestEnumSharesIsReadSafelyWhileSharesChange is the same for the share list, which a client walks
// through the srvsvc pipe while shares are registered and removed under it.
func TestEnumSharesIsReadSafelyWhileSharesChange(t *testing.T) {
	h := newSMBTest(t)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := range 500 {
			// What RegisterShare and RemoveShare do to the list, without the store behind them.
			h.srv.mu.Lock()
			if i%2 == 0 {
				h.srv.shareList["other"] = &share{name: "other", remark: "another share"}
			} else {
				delete(h.srv.shareList, "other")
			}
			h.srv.mu.Unlock()
		}
	}()

	go func() {
		defer wg.Done()
		for range 500 {
			h.srv.enumShares()
		}
	}()

	wg.Wait()
}

// TestWriteAccessFollowsTheRangeWritten is which right a write has to hold. [MS-SMB2] 3.3.5.13 asks
// for FILE_WRITE_DATA on a range that stays inside the file and FILE_APPEND_DATA on one that
// carries it past the end - one or the other, chosen by the range. Demanding both refuses every
// write through a handle that holds only one of them, and comparing the length of the write against
// the size of the file instead of the range it covers picks the wrong one.
func TestWriteAccessFollowsTheRangeWritten(t *testing.T) {
	const (
		writeOnly  = smb2.FILE_READ_DATA | smb2.FILE_WRITE_DATA
		appendOnly = smb2.FILE_READ_DATA | smb2.FILE_APPEND_DATA
	)

	for _, tt := range []struct {
		what   string
		access uint32
		offset uint64
		want   uint32
	}{
		// The file is 1024 bytes, and each write carries 512.
		{"inside the file, holding write access", writeOnly, 0, smb2.STATUS_OK},
		{"past the end, holding write access alone", writeOnly, 768, smb2.STATUS_ACCESS_DENIED},
		{"past the end, holding append access", appendOnly, 768, smb2.STATUS_OK},
		{"inside the file, holding append access alone", appendOnly, 0, smb2.STATUS_ACCESS_DENIED},
	} {
		t.Run(tt.what, func(t *testing.T) {
			h := newSMBTest(t)
			h.files.put("file", 1024)

			cl := h.dial("alice")

			// The rights of the share are what a handle on one of its files is granted.
			cl.tc.maximalAccess = tt.access

			handle, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
			fid := createdFileID(handle)

			cl.mid++
			resp, err := cl.send(writeRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, fid,
				tt.offset, bytes.Repeat([]byte("w"), 512)))
			if err != nil {
				t.Fatalf("the write failed: %v", err)
			}

			answer := resp.Encode()
			if resp.Header().Status() == smb2.STATUS_PENDING {
				answer = cl.recv(20 * time.Second)
			}

			if status := smb2.Header(answer).Status(); status != tt.want {
				t.Errorf("a write of 512 bytes at %d was answered %#x, want %#x",
					tt.offset, status, tt.want)
			}
		})
	}
}
