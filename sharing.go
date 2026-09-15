package main

import (
	"fmt"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// What an open does with a file, as the sharing rules weigh it. A client says what it means to do
// in the access it asks for, and what it will put up with from everybody else in the sharing mode:
// the two are held against each other in both directions, so that an open is refused whether it
// does something an open before it disallows, or disallows something an open before it does.
const (
	readsFile   = smb2.FILE_READ_DATA | smb2.FILE_EXECUTE | smb2.GENERIC_READ | smb2.GENERIC_EXECUTE | smb2.GENERIC_ALL
	writesFile  = smb2.FILE_WRITE_DATA | smb2.FILE_APPEND_DATA | smb2.GENERIC_WRITE | smb2.GENERIC_ALL
	deletesFile = smb2.DELETE | smb2.GENERIC_ALL

	// touchesFile is what an open has to do with a file for its sharing to be weighed at all.
	touchesFile = readsFile | writesFile | deletesFile
)

// sharingConflict reports whether two opens on one file can stand together. access and mode are
// what one of them does and allows, held and allowed the same of the other.
func sharingConflict(access, mode, held, allowed uint32) bool {
	// An open that neither reads, writes nor deletes the file takes no part in sharing: it is not
	// refused for it, and its mode keeps nobody out. Clients open a file for its attributes alone
	// under any mode at all, and a server that weighed those opens would refuse the opens around
	// them that the file system never would.
	if access&touchesFile == 0 || held&touchesFile == 0 {
		return false
	}

	// What this open means to do, against what the other one puts up with.
	if access&readsFile != 0 && allowed&smb2.FILE_SHARE_READ == 0 {
		return true
	}
	if access&writesFile != 0 && allowed&smb2.FILE_SHARE_WRITE == 0 {
		return true
	}
	if access&deletesFile != 0 && allowed&smb2.FILE_SHARE_DELETE == 0 {
		return true
	}

	// And the same the other way about: an open that allows nothing is refused for what the opens
	// already on the file are doing, not only for what it means to do itself.
	if held&readsFile != 0 && mode&smb2.FILE_SHARE_READ == 0 {
		return true
	}
	if held&writesFile != 0 && mode&smb2.FILE_SHARE_WRITE == 0 {
		return true
	}
	if held&deletesFile != 0 && mode&smb2.FILE_SHARE_DELETE == 0 {
		return true
	}

	return false
}

// sharingViolation returns the open that a create asking for access, and allowing mode of everybody
// else, cannot stand beside, or nil if the create may be answered.
//
// It is asked after the oplocks and the leases on the file have been broken, which is the order the
// holder of a batch oplock relies on: it is told to give the file up before the create it clashes
// with is refused, and the close it makes of that is what lets the create through on the retry
// ([FSBO] 2.3.1).
func (s *server) sharingViolation(sh *share, path string, access, mode uint32) *open {
	for _, other := range s.opensOn(sh, path, nil) {
		other.mu.Lock()
		held, allowed := other.grantedAccess, other.shareMode
		other.mu.Unlock()

		if sharingConflict(access, mode, held, allowed) {
			return other
		}
	}

	return nil
}

// describeSharingViolation says what a refused create collided with: what each of the two opens
// does and allows, and who holds the one standing in the way. An open kept for a connection that is
// gone blocks the file just the same, and telling it apart from a live one is most of what makes
// the refusal worth reading.
func describeSharingViolation(path string, access, mode uint32, other *open) string {
	other.mu.Lock()
	defer other.mu.Unlock()

	holder := "a live connection"
	if other.connection != nil && other.connection.clientName != "" {
		holder = other.connection.clientName
	}
	if !other.disconnectTime.IsZero() {
		holder = fmt.Sprintf("a durable open whose connection went %s ago", time.Since(other.disconnectTime).Round(time.Second))
	}

	return fmt.Sprintf("Sharing violation on %q: access %#x, sharing %#x refused beside an open with access %#x, sharing %#x, held by %s",
		path, access, mode, other.grantedAccess, other.shareMode, holder)
}
