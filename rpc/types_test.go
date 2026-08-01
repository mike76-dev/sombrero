package rpc

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/mike76-dev/sombrero/utils"
	"github.com/oiweiwei/go-msrpc/ndr"
)

// conformantString lays out a string the way NDR carries one: a maximum count, an offset into it,
// the count actually present and then that many sixteen-bit characters, the last of which
// terminates it.
func conformantString(s string) []byte {
	encoded := utils.EncodeStringToBytes(s)
	count := uint32(len(encoded)/2) + 1 // the characters and the one that ends them

	var buf []byte
	buf = binary.LittleEndian.AppendUint32(buf, count)
	buf = binary.LittleEndian.AppendUint32(buf, 0)
	buf = binary.LittleEndian.AppendUint32(buf, count)
	buf = append(buf, encoded...)
	buf = append(buf, 0, 0)

	return pad(buf)
}

// countedString lays out the plain-byte form the mdssvc call uses: a maximum count, the count
// present and then the bytes, terminated.
func countedString(s string) []byte {
	var buf []byte
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(s)+1))
	buf = binary.LittleEndian.AppendUint32(buf, 0)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(s)+1))
	buf = append(buf, []byte(s)...)
	buf = append(buf, 0)

	return pad(buf)
}

// pad takes a buffer up to the next four-byte boundary, which is where the next field starts.
func pad(buf []byte) []byte {
	return append(buf, make([]byte, utils.Roundup(len(buf), 4)-len(buf))...)
}

// netShareEnumAll builds the arguments of a NetShareEnumAll call.
func netShareEnumAll(server string, level, maxBuffer uint32) []byte {
	var buf []byte
	buf = binary.LittleEndian.AppendUint32(buf, 0x00020000) // the pointer to the server name
	buf = append(buf, conformantString(server)...)
	buf = binary.LittleEndian.AppendUint32(buf, level)
	buf = append(buf, make([]byte, 16)...) // the container the shares come back in
	buf = binary.LittleEndian.AppendUint32(buf, maxBuffer)

	return buf
}

// netShareGetInfo builds the arguments of a NetShareGetInfo call.
func netShareGetInfo(server, share string, level uint32) []byte {
	var buf []byte
	buf = binary.LittleEndian.AppendUint32(buf, 0x00020000) // the pointer to the server name
	buf = append(buf, conformantString(server)...)
	buf = append(buf, conformantString(share)...)
	buf = binary.LittleEndian.AppendUint32(buf, level)

	return buf
}

// mdsOpen builds the arguments of an MdsOpen call.
func mdsOpen(mountPath, shareName string) []byte {
	var buf []byte
	buf = binary.LittleEndian.AppendUint32(buf, 1)  // device
	buf = binary.LittleEndian.AppendUint32(buf, 2)  // unknown
	buf = binary.LittleEndian.AppendUint32(buf, 3)  // unknown
	buf = binary.LittleEndian.AppendUint32(buf, 64) // maximum count
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(mountPath)+1))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(mountPath)+1))
	buf = append(buf, []byte(mountPath)...)
	buf = append(buf, 0)
	buf = pad(buf)
	buf = append(buf, countedString(shareName)...)

	return buf
}

// TestNetShareEnumAllUnmarshal reads the arguments of the call a client makes to list the shares,
// which is the one that reaches this decoder in ordinary use.
func TestNetShareEnumAllUnmarshal(t *testing.T) {
	for _, tt := range []struct {
		name   string
		server string
	}{
		{"a server name", "\\\\sombrero"},
		{"a name of one character", "a"},
		{"a name with characters outside ASCII", "\\\\sömbrero"},
		{"no name at all", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var req NetShareEnumAllRequest
			if err := req.Unmarshal(netShareEnumAll(tt.server, 1, 0x1000)); err != nil {
				t.Fatalf("the call would not read: %v", err)
			}

			if req.Server != tt.server {
				t.Errorf("the server came out %q, want %q", req.Server, tt.server)
			}
			if req.Level != 1 {
				t.Errorf("the level came out %d, want 1", req.Level)
			}
			if req.MaxBuffer != 0x1000 {
				t.Errorf("the buffer size came out %#x, want %#x", req.MaxBuffer, 0x1000)
			}
		})
	}
}

// TestNetShareGetInfoUnmarshal reads the arguments of the call asking about one share.
func TestNetShareGetInfoUnmarshal(t *testing.T) {
	var req NetShareGetInfoRequest
	if err := req.Unmarshal(netShareGetInfo("\\\\sombrero", "documents", 1)); err != nil {
		t.Fatalf("the call would not read: %v", err)
	}

	if req.Server != "\\\\sombrero" {
		t.Errorf("the server came out %q", req.Server)
	}
	if req.Share != "documents" {
		t.Errorf("the share came out %q, want %q", req.Share, "documents")
	}
	if req.Level != 1 {
		t.Errorf("the level came out %d, want 1", req.Level)
	}
}

// TestMdsOpenUnmarshal reads the arguments of the call the Spotlight client opens with.
func TestMdsOpenUnmarshal(t *testing.T) {
	var req MdsOpenRequest
	if err := req.Unmarshal(mdsOpen("/Volumes/documents", "documents")); err != nil {
		t.Fatalf("the call would not read: %v", err)
	}

	if req.DeviceID != 1 {
		t.Errorf("the device came out %d, want 1", req.DeviceID)
	}
	if req.MaxCount != 64 {
		t.Errorf("the maximum count came out %d, want 64", req.MaxCount)
	}
	if req.ShareMountPath != "/Volumes/documents" {
		t.Errorf("the mount path came out %q", req.ShareMountPath)
	}
	if req.ShareName != "documents" {
		t.Errorf("the share name came out %q, want %q", req.ShareName, "documents")
	}
}

// TestUnmarshalRefusesWhatRunsPastTheEnd is what these three decoders are really about. Each is
// handed the payload of a request over a named pipe, and every length in it arrives from the far
// end. Not one of these buffers was survivable: reading nothing at all crashed all three, a
// length field of zero crashed all three by taking the terminator off a string that has none, and
// a length near the top of its own field wrapped round and named a range back inside the buffer.
//
// A crash here is not the connection going away. There is no recover anywhere in the read path,
// so it is the process, and every other client's sessions go with it.
func TestUnmarshalRefusesWhatRunsPastTheEnd(t *testing.T) {
	// A length field of zero, which the slicing then takes the terminator off.
	zeroed := make([]byte, 64)

	// A length field at the top of its range, which doubled wraps round.
	wrapping := make([]byte, 64)
	for i := range wrapping {
		wrapping[i] = 0xff
	}

	// A length saying the string is longer than the buffer holding it.
	overlong := make([]byte, 64)
	binary.LittleEndian.PutUint32(overlong[8:12], 0x1000)
	binary.LittleEndian.PutUint32(overlong[12:16], 0x1000)
	binary.LittleEndian.PutUint32(overlong[20:24], 0x1000)

	for _, tt := range []struct {
		name string
		buf  []byte
	}{
		{"nothing at all", nil},
		{"an empty payload", []byte{}},
		{"a single byte", []byte{0}},
		{"four bytes", make([]byte, 4)},
		{"a header and nothing behind it", make([]byte, 12)},
		{"lengths of zero", zeroed},
		{"lengths at the top of their range", wrapping},
		{"a length longer than what arrived", overlong},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// None of the three may end the process; each must say what it could not read.
			var enumAll NetShareEnumAllRequest
			if err := enumAll.Unmarshal(tt.buf); err == nil {
				t.Log("NetShareEnumAll read it without complaint")
			}

			var getInfo NetShareGetInfoRequest
			if err := getInfo.Unmarshal(tt.buf); err == nil {
				t.Log("NetShareGetInfo read it without complaint")
			}

			var open MdsOpenRequest
			if err := open.Unmarshal(tt.buf); err == nil {
				t.Log("MdsOpen read it without complaint")
			}
		})
	}
}

// TestUnmarshalRefusesACallCutShort walks each well-formed call, cutting it at every length in
// turn. Two things have to hold. A prefix short of what the call reads must be reported rather
// than read past; and once enough has arrived the answer must be the one the whole call gives,
// since what is left at the end is padding to the next boundary rather than anything read.
//
// The point the two meet is a little short of the whole for that reason, and where it falls is
// not asserted — only that it exists, that nothing below it is accepted, and that nothing above
// it changes its mind.
func TestUnmarshalRefusesACallCutShort(t *testing.T) {
	for _, tt := range []struct {
		name  string
		buf   []byte
		parse func(buf []byte) (any, error)
	}{
		{
			name: "NetShareEnumAll",
			buf:  netShareEnumAll("\\\\sombrero", 1, 0x1000),
			parse: func(buf []byte) (any, error) {
				var req NetShareEnumAllRequest
				err := req.Unmarshal(buf)
				return req, err
			},
		},
		{
			name: "NetShareGetInfo",
			buf:  netShareGetInfo("\\\\sombrero", "documents", 1),
			parse: func(buf []byte) (any, error) {
				var req NetShareGetInfoRequest
				err := req.Unmarshal(buf)
				return req, err
			},
		},
		{
			name: "MdsOpen",
			buf:  mdsOpen("/Volumes/documents", "documents"),
			parse: func(buf []byte) (any, error) {
				var req MdsOpenRequest
				err := req.Unmarshal(buf)
				return req, err
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			whole, err := tt.parse(tt.buf)
			if err != nil {
				t.Fatalf("the whole call would not read: %v", err)
			}

			var accepted int = -1
			for at := 0; at <= len(tt.buf); at++ {
				got, err := tt.parse(tt.buf[:at])

				if err != nil {
					if accepted >= 0 {
						t.Errorf("cut at %d it was refused, having been read at %d", at, accepted)
					}
					continue
				}

				if accepted < 0 {
					accepted = at
				}
				if got != whole {
					t.Errorf("cut at %d it read as %+v, want the %+v of the whole call", at, got, whole)
				}
			}

			if accepted < 0 {
				t.Fatal("no length of this call was ever read")
			}
			t.Logf("read from %d bytes of %d; the rest is padding", accepted, len(tt.buf))
		})
	}
}

// TestNetShareInfo1ResponseMarshals is the answer to a call about one share, checked for the
// length it says it is rather than for its exact bytes: the name and the remark are written in
// with counts in front of them, and a count that disagreed with what follows it would have the
// client read the next field out of the middle of this one.
func TestNetShareInfo1ResponseMarshals(t *testing.T) {
	for _, tt := range []struct {
		name           string
		share, comment string
	}{
		{"a plain share", "documents", "the documents"},
		{"nothing for a remark", "documents", ""},
		{"a name of one character", "a", "b"},
		{"characters outside ASCII", "dökumente", "die dökumente"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resp := &NetShareInfo1Response{
				NetShareInfo1: NetShareInfo1{Share: tt.share, Comment: tt.comment},
				Result:        0,
			}

			out, err := ndr.Marshal(resp)
			if err != nil {
				t.Fatalf("the response would not go out: %v", err)
			}

			// Everything in the structure sits on a four-byte boundary, so the whole of it
			// has to come out a multiple of four long.
			if len(out)%4 != 0 {
				t.Errorf("the response came out %d bytes long, which is not a whole number of words", len(out))
			}
			if !bytes.Contains(out, utils.EncodeStringToBytes(tt.share)) {
				t.Error("the name of the share is not in what went out")
			}
		})
	}
}

// TestNetShareEnumAllResponseMarshals is the answer listing every share, which is what a client
// draws its list of shares from.
func TestNetShareEnumAllResponseMarshals(t *testing.T) {
	for _, count := range []int{0, 1, 2, 16} {
		shares := make([]NetShareInfo1, count)
		for i := range shares {
			shares[i] = NetShareInfo1{Share: "documents", Comment: "the documents"}
		}

		resp := &NetShareEnumAllResponse{Shares: shares, Result: 0}
		out, err := ndr.Marshal(resp)
		if err != nil {
			t.Fatalf("a list of %d shares would not go out: %v", count, err)
		}

		if len(out)%4 != 0 {
			t.Errorf("a list of %d shares came out %d bytes long, which is not a whole number of words", count, len(out))
		}

		// The count is written out at the front, and again at the back where the client reads
		// how many it was actually given.
		if got := binary.LittleEndian.Uint32(out[12:16]); int(got) != count {
			t.Errorf("the list says it holds %d shares, want %d", got, count)
		}
	}
}

// FuzzUnmarshal walks the payload of a call over a named pipe. The property is only that an
// answer comes back: every one of these decoders is handed bytes a client wrote, and a panic in
// any of them ends the process rather than the connection.
func FuzzUnmarshal(f *testing.F) {
	f.Add(netShareEnumAll("\\\\sombrero", 1, 0x1000))
	f.Add(netShareGetInfo("\\\\sombrero", "documents", 1))
	f.Add(mdsOpen("/Volumes/documents", "documents"))
	f.Add(make([]byte, 64))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, buf []byte) {
		var enumAll NetShareEnumAllRequest
		_ = enumAll.Unmarshal(buf)

		var getInfo NetShareGetInfoRequest
		_ = getInfo.Unmarshal(buf)

		var open MdsOpenRequest
		_ = open.Unmarshal(buf)
	})
}
