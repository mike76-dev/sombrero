package smb2

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/ntlm"
)

// TestNewSecInfoWithNoDomainBehindTheSession is the descriptor built for a session that carries no
// domain identifier: an anonymous one, or any context that was never filled in. The owner is the
// domain with the user's identifier on the end of it, and reaching for the domain of a session
// that has none brought the whole connection down.
func TestNewSecInfoWithNoDomainBehindTheSession(t *testing.T) {
	for _, tt := range []struct {
		what string
		ctx  ntlm.SecurityContext
		want uint32
	}{
		{"a context with nothing in it", ntlm.SecurityContext{}, 0},
		{"an anonymous session", ntlm.AnonymousContext(), 7},
	} {
		t.Run(tt.what, func(t *testing.T) {
			info := NewSecInfo(tt.ctx, OWNER_SECURITY_INFORMATION|GROUP_SECURITY_INFORMATION|
				DACL_SECURITY_INFORMATION, FILE_READ_DATA)
			if len(info) < 32 {
				t.Fatalf("the descriptor is %d bytes long, too short to hold an owner", len(info))
			}

			// The owner SID follows the twenty-byte header: a revision, the count of
			// sub-authorities, the six-byte authority, and then the sub-authorities.
			if n := info[21]; n != 1 {
				t.Fatalf("the owner carries %d sub-authorities, want the identifier standing alone", n)
			}
			if rid := binary.LittleEndian.Uint32(info[28:32]); rid != tt.want {
				t.Errorf("the owner is S-1-5-%d, want S-1-5-%d", rid, tt.want)
			}
		})
	}
}

// TestQueryDirectoryBufferStaysWithinTheBufferForEveryClass is a listing of long names, answered
// in each information class, against a buffer the entries fill to the brim. The room an entry
// takes was budgeted at the width of one class, and the wider ones ran past what the client set
// aside once enough entries went in to use up the slack.
func TestQueryDirectoryBufferStaysWithinTheBufferForEveryClass(t *testing.T) {
	var entries []client.ObjectInfo
	for i := range 200 {
		entries = append(entries, client.ObjectInfo{Key: fmt.Sprintf("/dir/file-%03d-%s.txt", i, strings.Repeat("x", 30))})
	}
	const bufSize = 224 + 184*100

	for _, tt := range []struct {
		name  string
		class uint8
	}{
		{"FILE_DIRECTORY_INFORMATION", FILE_DIRECTORY_INFORMATION},
		{"FILE_FULL_DIRECTORY_INFORMATION", FILE_FULL_DIRECTORY_INFORMATION},
		{"FILE_ID_FULL_DIRECTORY_INFORMATION", FILE_ID_FULL_DIRECTORY_INFORMATION},
		{"FILE_ID_64_EXTD_DIRECTORY_INFORMATION", FILE_ID_64_EXTD_DIRECTORY_INFORMATION},
		{"FILE_ID_EXTD_DIRECTORY_INFORMATION", FILE_ID_EXTD_DIRECTORY_INFORMATION},
		{"FILE_BOTH_DIRECTORY_INFORMATION", FILE_BOTH_DIRECTORY_INFORMATION},
		{"FILE_ID_ALL_EXTD_DIRECTORY_INFORMATION", FILE_ID_ALL_EXTD_DIRECTORY_INFORMATION},
		{"FILE_ID_BOTH_DIRECTORY_INFORMATION", FILE_ID_BOTH_DIRECTORY_INFORMATION},
		{"FILE_ID_64_EXTD_BOTH_DIRECTORY_INFORMATION", FILE_ID_64_EXTD_BOTH_DIRECTORY_INFORMATION},
		{"FILE_ID_ALL_EXTD_BOTH_DIRECTORY_INFORMATION", FILE_ID_ALL_EXTD_BOTH_DIRECTORY_INFORMATION},
	} {
		t.Run(tt.name, func(t *testing.T) {
			buf, num := QueryDirectoryBuffer(tt.class, entries, bufSize, false, true, client.FileInfo{}, client.FileInfo{})
			if num == 0 {
				t.Fatal("nothing was listed into a buffer with room for dozens of entries")
			}
			if len(buf) > bufSize {
				t.Fatalf("%d entries were encoded into %d bytes, past the %d the client set aside", num, len(buf), bufSize)
			}
		})
	}
}
