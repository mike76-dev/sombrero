package main

import "github.com/mike76-dev/sombrero/smb2"

// What an open does with a file, as the sharing rules weigh it. A client says what it means to do
// in the access it asks for, and what it will put up with from everybody else in the sharing mode:
// the two are held against each other in both directions, so that an open is refused whether it
// does something an open before it disallows, or disallows something an open before it does.
const (
	readsFile   = smb2.FILE_READ_DATA | smb2.FILE_EXECUTE | smb2.GENERIC_READ | smb2.GENERIC_EXECUTE | smb2.GENERIC_ALL
	writesFile  = smb2.FILE_WRITE_DATA | smb2.FILE_APPEND_DATA | smb2.GENERIC_WRITE | smb2.GENERIC_ALL
	deletesFile = smb2.DELETE | smb2.GENERIC_ALL
)

// sharingConflict reports whether two opens on one file can stand together. access and mode are
// what one of them does and allows, held and allowed the same of the other.
func sharingConflict(access, mode, held, allowed uint32) bool {
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

// sharingViolation reports whether a create asking for access, and allowing mode of everybody else,
// may be answered at all: an open of the file already stands that it cannot stand beside.
//
// It is asked after the oplocks and the leases on the file have been broken, which is the order the
// holder of a batch oplock relies on: it is told to give the file up before the create it clashes
// with is refused, and the close it makes of that is what lets the create through on the retry
// ([FSBO] 2.3.1).
func (s *server) sharingViolation(sh *share, path string, access, mode uint32) bool {
	for _, other := range s.opensOn(sh, path, nil) {
		other.mu.Lock()
		held, allowed := other.grantedAccess, other.shareMode
		other.mu.Unlock()

		if sharingConflict(access, mode, held, allowed) {
			return true
		}
	}

	return false
}
