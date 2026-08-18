package main

import (
	"bytes"
	"errors"
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

			// A handle is granted what its create asked for, as far as the share allows, so
			// the right under test is both what the share holds and what is asked of it.
			cl.tc.maximalAccess = tt.access

			handle, err := cl.createAccessing("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, tt.access, nil)
			if err != nil {
				t.Fatalf("the create failed: %v", err)
			}
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

// TestAHandleIsGrantedWhatItsCreateAsksFor is what an open carries once it is made. [MS-SMB2]
// 3.3.5.9 grants a handle the access its create asked for, as far as the share allows it. The
// access of the tree connect was handed over whole instead, so a client that opened a file for
// reading held one it could write, rename and delete through.
func TestAHandleIsGrantedWhatItsCreateAsksFor(t *testing.T) {
	for _, tt := range []struct {
		what   string
		asked  uint32
		want   uint32
		writes bool
	}{
		{"reading alone", readAccess, readAccess, false},
		{"reading and writing", writeAccess, writeAccess, true},
		{"everything the user may do", smb2.MAXIMUM_ALLOWED, shareAccess, true},
		{"the generic rights", smb2.GENERIC_READ | smb2.GENERIC_WRITE, shareAccess &^ smb2.DELETE, true},
	} {
		t.Run(tt.what, func(t *testing.T) {
			h := newSMBTest(t)
			h.files.put("file", 1024)

			cl := h.dial("alice")
			handle, err := cl.createAccessing("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, tt.asked, nil)
			if err != nil {
				t.Fatalf("the create failed: %v", err)
			}

			op := h.srv.globalOpenTable[openIDOf(createdFileID(handle))]
			if op == nil {
				t.Fatal("the create left no open behind")
			}
			if ga := op.grantedAccess; ga != tt.want {
				t.Errorf("a create asking for %#x was granted %#x, want %#x", tt.asked, ga, tt.want)
			}

			// What the handle is granted is what the writing through it is weighed against.
			cl.mid++
			resp, err := cl.send(writeRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID,
				createdFileID(handle), 0, bytes.Repeat([]byte("w"), 512)))
			if err != nil {
				t.Fatalf("the write failed: %v", err)
			}

			answer := resp.Encode()
			if resp.Header().Status() == smb2.STATUS_PENDING {
				answer = cl.recv(20 * time.Second)
			}

			want := uint32(smb2.STATUS_ACCESS_DENIED)
			if tt.writes {
				want = smb2.STATUS_OK
			}
			if status := smb2.Header(answer).Status(); status != want {
				t.Errorf("a write through a handle asking for %#x was answered %#x, want %#x",
					tt.asked, status, want)
			}
		})
	}
}

// TestMaximalAccessIsReportedOverTheFile is what the create context of that name answers with.
// It is asked what the user may do with the file, which is the access of the tree connect, and
// not what this one handle happens to have been granted ([MS-SMB2] 3.3.5.9.5).
func TestMaximalAccessIsReportedOverTheFile(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")

	// A handle deliberately granted less than the share allows.
	handle, err := cl.createAccessing("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, readAccess,
		maximalAccessContext())
	if err != nil {
		t.Fatalf("the create failed: %v", err)
	}

	access, found := createdMaximalAccess(handle)
	if !found {
		t.Fatal("the create was not answered with a maximal access context")
	}
	if access != shareAccess {
		t.Errorf("the maximal access over the file is reported as %#x, want %#x", access, shareAccess)
	}
}

// TestACreateAskingForMoreThanItMayHaveIsRefused is when a create is turned away. The rights asked
// for were weighed by whether any one of them was held, so a create that asked to read and write a
// file the user may only read was let through, and the client met the refusal at its first write
// instead. Windows fails the open itself, which is where a client that cannot go on expects it.
func TestACreateAskingForMoreThanItMayHaveIsRefused(t *testing.T) {
	var (
		storedRead  = stores.FlagsFromAccessRights(stores.AccessRights{ReadAccess: true})
		storedWrite = stores.FlagsFromAccessRights(stores.AccessRights{WriteAccess: true})
	)

	for _, tt := range []struct {
		what    string
		granted uint32
		asked   uint32
		want    uint32
	}{
		{"what it holds", readAccess, readAccess, smb2.STATUS_OK},
		{"more than it holds", readAccess, readAccess | smb2.FILE_WRITE_DATA, smb2.STATUS_ACCESS_DENIED},
		{"one right beyond the rest", storedRead | storedWrite, smb2.FILE_READ_DATA | smb2.DELETE, smb2.STATUS_ACCESS_DENIED},
		{"whatever there is", readAccess, smb2.MAXIMUM_ALLOWED, smb2.STATUS_OK},
		{"generic rights it holds", storedRead | storedWrite, smb2.GENERIC_READ | smb2.GENERIC_WRITE, smb2.STATUS_OK},
		{"generic rights it does not hold", storedRead, smb2.GENERIC_WRITE, smb2.STATUS_ACCESS_DENIED},

		// The stored rights hand out no SYNCHRONIZE with write access, and a client asks for it
		// with every open it makes.
		{"the right to wait on the handle", storedWrite, smb2.FILE_WRITE_DATA | smb2.SYNCHRONIZE, smb2.STATUS_OK},
	} {
		t.Run(tt.what, func(t *testing.T) {
			h := newSMBTest(t)
			h.files.put("file", 1024)

			// The share has to hold security for anything to be weighed against, and the tree
			// connect carries what it holds.
			h.restrictTo("alice")
			h.share.fileSecurity[h.workgroup+"/alice"] = tt.granted

			cl := h.dial("alice")
			cl.tc.maximalAccess = tt.granted

			resp, err := cl.createAccessing("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, tt.asked, nil)
			if err != nil {
				t.Fatalf("the create failed: %v", err)
			}

			if status := smb2.Header(resp).Status(); status != tt.want {
				t.Errorf("a create asking for %#x against rights of %#x was answered %#x, want %#x",
					tt.asked, tt.granted, status, tt.want)
			}
		})
	}
}

// TestADispositionThatChangesTheFileNeedsWriteAccess is which opens a read-only user may make.
// A create disposition is one of a numbered set, and it was tested as though it were a set of
// bits: FILE_OPEN, which changes nothing, came out of that mask looking like a write and was
// refused, while FILE_SUPERSEDE, which replaces the file outright, came out looking like none
// and was allowed.
func TestADispositionThatChangesTheFileNeedsWriteAccess(t *testing.T) {
	for _, tt := range []struct {
		what        string
		disposition uint32
		want        uint32
	}{
		{"opening it", smb2.FILE_OPEN, smb2.STATUS_OK},
		{"superseding it", smb2.FILE_SUPERSEDE, smb2.STATUS_ACCESS_DENIED},
		{"overwriting it", smb2.FILE_OVERWRITE, smb2.STATUS_ACCESS_DENIED},
		{"creating it", smb2.FILE_CREATE, smb2.STATUS_ACCESS_DENIED},
		{"opening it or creating it", smb2.FILE_OPEN_IF, smb2.STATUS_ACCESS_DENIED},
	} {
		t.Run(tt.what, func(t *testing.T) {
			h := newSMBTest(t)
			h.files.put("file", 1024)

			h.restrictTo("alice")
			h.share.fileSecurity[h.workgroup+"/alice"] = readAccess

			cl := h.dial("alice")
			cl.tc.maximalAccess = readAccess

			resp, err := cl.createAccessing("file", smb2.OPLOCK_LEVEL_NONE, tt.disposition, readAccess, nil)
			if err != nil {
				t.Fatalf("the create failed: %v", err)
			}

			if status := smb2.Header(resp).Status(); status != tt.want {
				t.Errorf("a read-only user %s was answered %#x, want %#x", tt.what, status, tt.want)
			}
		})
	}
}

// TestAShareWithNoSecurityGrantsNothing is the empty security table, and what the two halves of
// the server make of it. A tree connect on such a share is refused, because the user holds no
// rights on it; the create went the other way and read an empty table as a share nobody is kept
// out of, so the one path that could still reach a file gave every user every right over it.
func TestAShareWithNoSecurityGrantsNothing(t *testing.T) {
	for _, tt := range []struct {
		what  string
		empty func(sh *share)
	}{
		{"tables that were never filled in", func(sh *share) {
			sh.connectSecurity = nil
			sh.fileSecurity = nil
		}},
		{"tables that hold nobody", func(sh *share) {
			sh.connectSecurity = make(map[string]struct{})
			sh.fileSecurity = make(map[string]uint32)
		}},
	} {
		t.Run(tt.what, func(t *testing.T) {
			h := newSMBTest(t)
			h.files.put("file", 1024)

			cl := h.dial("alice")
			tt.empty(h.share)

			resp, err := cl.createErr("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
			if err != nil {
				t.Fatalf("the create failed: %v", err)
			}
			if status := smb2.Header(resp).Status(); status != smb2.STATUS_ACCESS_DENIED {
				t.Errorf("a create on a share holding no security was answered %#x, want STATUS_ACCESS_DENIED", status)
			}

			// The half that reaches the share the other way round, which has always said so.
			if _, err := cl.conn.newTreeConnect(cl.ss, `\\SERVER\files`); !errors.Is(err, errAccessDenied) {
				t.Errorf("a tree connect on the same share was refused with %v, want it denied", err)
			}
		})
	}
}
