package smb2

import (
	"encoding/binary"
	"testing"
	"time"
)

// closeRequestWithFlags builds the bytes of an SMB2_CLOSE request carrying the given Flags field.
func closeRequestWithFlags(flags uint16) CloseRequest {
	msg := make([]byte, SMB2HeaderSize+SMB2CloseRequestMinSize)
	h := NewHeader(msg)
	h.SetCommand(SMB2_CLOSE)

	body := msg[SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[0:2], SMB2CloseRequestStructureSize)
	binary.LittleEndian.PutUint16(body[2:4], flags)

	return CloseRequest{Request{data: msg}}
}

// TestCloseAnswersThePostqueryBit is which clients get the attributes of the file they just closed.
// SMB2_CLOSE_FLAG_POSTQUERY_ATTRIB is a bit in the Flags field, and [MS-SMB2] 3.3.5.10 asks whether
// it is set - not whether it is the whole of the field. Compared for equality, a client that sets
// anything alongside it is answered with a response full of zeros, and reads its file as having no
// size and no timestamps.
func TestCloseAnswersThePostqueryBit(t *testing.T) {
	modified := time.Unix(1_700_000_000, 0).UTC()

	for _, tt := range []struct {
		what  string
		flags uint16
		want  bool
	}{
		{"the bit on its own", CLOSE_FLAG_POSTQUERY_ATTRIB, true},
		{"the bit beside another", CLOSE_FLAG_POSTQUERY_ATTRIB | 0x0002, true},
		{"nothing at all", 0, false},
		{"another bit on its own", 0x0002, false},
	} {
		t.Run(tt.what, func(t *testing.T) {
			resp := &CloseResponse{}
			resp.FromRequest(closeRequestWithFlags(tt.flags))
			resp.Generate(modified, 1024, 1024, FILE_ATTRIBUTE_NORMAL)

			buf := resp.Encode()
			gotFlags := binary.LittleEndian.Uint16(buf[SMB2HeaderSize+2:SMB2HeaderSize+4]) & CLOSE_FLAG_POSTQUERY_ATTRIB
			gotSize := binary.LittleEndian.Uint64(buf[SMB2HeaderSize+40 : SMB2HeaderSize+48])

			if answered := gotFlags != 0; answered != tt.want {
				t.Errorf("the response says POSTQUERY_ATTRIB = %v, want %v", answered, tt.want)
			}
			if carried := gotSize != 0; carried != tt.want {
				t.Errorf("the response carries a size of %d, want it filled in = %v", gotSize, tt.want)
			}
		})
	}
}
