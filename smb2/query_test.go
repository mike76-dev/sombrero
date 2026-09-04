package smb2

import (
	"encoding/binary"
	"testing"

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
