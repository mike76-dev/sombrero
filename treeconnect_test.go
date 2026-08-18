package main

import (
	"testing"

	"github.com/mike76-dev/sombrero/smb2"
)

// TestTreeConnectAnswersWithTheStatusTheSpecNames checks what a refused tree connect is told.
// [MS-SMB2] 3.3.5.7 names a status for each way it can be refused, and they are not
// interchangeable: a client that is told SHARE_UNAVAILABLE where the spec says BAD_NETWORK_NAME
// keeps a name it should have given up on, and one told INVALID_PARAMETER where the spec says
// REQUEST_NOT_ACCEPTED treats a share that is merely busy as a share it asked for wrongly.
func TestTreeConnectAnswersWithTheStatusTheSpecNames(t *testing.T) {
	for _, tt := range []struct {
		what  string
		path  string
		setUp func(h *smbTest)
		want  uint32
	}{
		{
			what: "a share that does not exist",
			path: `\\SERVER\nosuch`,
			want: smb2.STATUS_BAD_NETWORK_NAME,
		},
		{
			what: "a path with no share name in it",
			path: `\\SERVER\`,
			want: smb2.STATUS_INVALID_PARAMETER,
		},
		{
			what: "a share already at its limit of uses",
			path: `\\SERVER\files`,
			setUp: func(h *smbTest) {
				h.share.currentUses = maxShareUses
			},
			want: smb2.STATUS_REQUEST_NOT_ACCEPTED,
		},
	} {
		t.Run(tt.what, func(t *testing.T) {
			h := newSMBTest(t)
			if tt.setUp != nil {
				tt.setUp(h)
			}

			// Below 3.1.1, where an unsigned tree connect is answered rather than a reason to drop
			// the connection.
			cl := h.dial("alice").speaking(smb2.SMB_DIALECT_302)

			resp, _, err := cl.conn.processRequest(request(t,
				treeConnectRequest(0, cl.ss.sessionID, tt.path)))
			if err != nil {
				t.Fatalf("the tree connect was not answered: %v", err)
			}
			if status := resp.Header().Status(); status != tt.want {
				t.Errorf("the tree connect was answered %#x, want %#x", status, tt.want)
			}
		})
	}
}
