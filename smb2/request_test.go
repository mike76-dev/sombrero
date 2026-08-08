package smb2

import (
	"encoding/binary"
	"testing"
)

// header builds a bare, well-formed SMB2 header, which is the least a message has to be for the
// parsing to look at anything behind it.
func header(next uint32) []byte {
	msg := make([]byte, SMB2HeaderSize)
	h := NewHeader(msg)
	h.SetCommand(SMB2_ECHO)
	h.SetNextCommand(next)

	return msg
}

// TestGetRequestsRefusesAChainRunningPastTheEnd is the message whose NextCommand points beyond
// the bytes that arrived. The field is read straight off the wire before anybody has
// authenticated, so what it says has to be measured against the message rather than trusted:
// taking it at its word reaches past the end of the buffer.
func TestGetRequestsRefusesAChainRunningPastTheEnd(t *testing.T) {
	for _, tt := range []struct {
		name string
		next uint32
	}{
		{"just past the end", SMB2HeaderSize + 8},
		{"far past the end", 0x30303030},
		{"as far as the field goes", ^uint32(0) &^ 7},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := GetRequests(header(tt.next), 0, 0, false); err == nil {
				t.Fatal("a chain that runs past the end of the message was accepted")
			}
		})
	}
}

// TestGetRequestsTakesAWholeChain is the control: a chain that fits is still parsed into the
// requests it holds.
func TestGetRequestsTakesAWholeChain(t *testing.T) {
	msg := append(header(SMB2HeaderSize), header(0)...)

	reqs, err := GetRequests(msg, 0, 0, false)
	if err != nil {
		t.Fatalf("a chain that fits was refused: %v", err)
	}
	if len(reqs) != 2 {
		t.Fatalf("the chain came apart into %d requests, want 2", len(reqs))
	}
	if got := reqs[0].Len(); got != SMB2HeaderSize {
		t.Fatalf("the first request is %d bytes long, want %d", got, SMB2HeaderSize)
	}
}

// message builds a well-formed request of the given command, long enough to hold its fixed part
// and a little room behind it, with the structure size the parsing insists on.
func message(command uint16, structureSize uint16, minSize int) []byte {
	msg := make([]byte, SMB2HeaderSize+minSize+32)
	h := NewHeader(msg)
	h.SetCommand(command)
	h.SetCreditCharge(1)
	binary.LittleEndian.PutUint16(msg[SMB2HeaderSize:SMB2HeaderSize+2], structureSize)

	return msg
}

// putU16 and putU32 write a field of the request body, counted from the start of the body.
func putU16(msg []byte, at int, v uint16) {
	binary.LittleEndian.PutUint16(msg[SMB2HeaderSize+at:SMB2HeaderSize+at+2], v)
}

func putU32(msg []byte, at int, v uint32) {
	binary.LittleEndian.PutUint32(msg[SMB2HeaderSize+at:SMB2HeaderSize+at+4], v)
}

// TestRequestFieldsRunningPastTheEnd is the offset and the length of a field that between them
// carry past the top of the width they are counted in. Added up in that width the pair comes out
// as a small number, which passes for a field well inside the message; taken at that word, the
// bytes it names are reached for far outside it.
func TestRequestFieldsRunningPastTheEnd(t *testing.T) {
	for _, tt := range []struct {
		name string
		// build lays out a request whose field runs past the end of the message.
		build func() []byte
		// read validates the request the way the server does and then reads the field.
		read func(t *testing.T, data []byte)
	}{
		{
			name: "session setup security buffer",
			build: func() []byte {
				msg := message(SMB2_SESSION_SETUP, SMB2SessionSetupRequestStructureSize, SMB2SessionSetupRequestMinSize)
				putU16(msg, 12, ^uint16(0))
				putU16(msg, 14, 2)
				return msg
			},
			read: func(t *testing.T, data []byte) {
				ssr := SessionSetupRequest{Request: Request{data: data}}
				if ssr.Validate(true) != nil {
					return
				}
				if buf := ssr.SecurityBuffer(); len(buf) != 0 {
					t.Errorf("the field came back with %d bytes in it", len(buf))
				}
			},
		},
		{
			name: "tree connect path name",
			build: func() []byte {
				msg := message(SMB2_TREE_CONNECT, SMB2TreeConnectRequestStructureSize, SMB2TreeConnectRequestMinSize)
				putU16(msg, 4, ^uint16(0))
				putU16(msg, 6, 2)
				return msg
			},
			read: func(t *testing.T, data []byte) {
				tcr := TreeConnectRequest{Request: Request{data: data}}
				if tcr.Validate(true) != nil {
					return
				}
				if name := tcr.PathName(); name != "" {
					t.Errorf("the field came back as %q", name)
				}
			},
		},
		{
			name: "create file name",
			build: func() []byte {
				msg := message(SMB2_CREATE, SMB2CreateRequestStructureSize, SMB2CreateRequestMinSize)
				putU16(msg, 44, ^uint16(0)&^7)
				putU16(msg, 46, 8)
				return msg
			},
			read: func(t *testing.T, data []byte) {
				cr := CreateRequest{Request: Request{data: data}}
				if cr.Validate(true) != nil {
					return
				}
				if name := cr.Filename(); name != "" {
					t.Errorf("the field came back as %q", name)
				}
			},
		},
		{
			// The chain begins inside the message and its header does not fit in what is left,
			// which is the shape the walk has to answer for: the offset it starts from passes
			// every check made on the chain as a whole.
			name: "create context header past the end",
			build: func() []byte {
				msg := message(SMB2_CREATE, SMB2CreateRequestStructureSize, SMB2CreateRequestMinSize)
				putU32(msg, 48, uint32(len(msg))-4)
				putU32(msg, 52, 4)
				return msg
			},
			read: func(t *testing.T, data []byte) {
				cr := CreateRequest{Request: Request{data: data}}
				if cr.Validate(true) != nil {
					return
				}
				if ctxs, err := cr.CreateContexts(); err == nil && len(ctxs) > 0 {
					t.Errorf("%d contexts came back out of a chain that runs past the end", len(ctxs))
				}
			},
		},
		{
			name: "create context chain carrying past the count",
			build: func() []byte {
				msg := message(SMB2_CREATE, SMB2CreateRequestStructureSize, SMB2CreateRequestMinSize)
				putU32(msg, 48, ^uint32(0)-16)
				putU32(msg, 52, 32)
				return msg
			},
			read: func(t *testing.T, data []byte) {
				cr := CreateRequest{Request: Request{data: data}}
				if cr.Validate(true) != nil {
					return
				}
				if ctxs, err := cr.CreateContexts(); err == nil && len(ctxs) > 0 {
					t.Errorf("%d contexts came back out of a chain that runs past the end", len(ctxs))
				}
			},
		},
		{
			name: "write buffer",
			build: func() []byte {
				msg := message(SMB2_WRITE, SMB2WriteRequestStructureSize, SMB2WriteRequestMinSize)
				putU16(msg, 2, ^uint16(0))
				putU32(msg, 4, ^uint32(0)-uint32(^uint16(0))+2)
				return msg
			},
			read: func(t *testing.T, data []byte) {
				wr := WriteRequest{Request: Request{data: data}}
				if wr.Validate(true) != nil {
					return
				}
				if buf := wr.Buffer(); len(buf) != 0 {
					t.Errorf("the field came back with %d bytes in it", len(buf))
				}
			},
		},
		{
			name: "set info buffer",
			build: func() []byte {
				msg := message(SMB2_SET_INFO, SMB2SetInfoRequestStructureSize, SMB2SetInfoRequestMinSize)
				putU16(msg, 8, ^uint16(0))
				putU32(msg, 4, ^uint32(0)-uint32(^uint16(0))+2)
				return msg
			},
			read: func(t *testing.T, data []byte) {
				sir := SetInfoRequest{Request: Request{data: data}}
				if sir.Validate(true) != nil {
					return
				}
				if buf := sir.Buffer(); len(buf) != 0 {
					t.Errorf("the field came back with %d bytes in it", len(buf))
				}
			},
		},
		{
			name: "query info input buffer",
			build: func() []byte {
				msg := message(SMB2_QUERY_INFO, SMB2QueryInfoRequestStructureSize, SMB2QueryInfoRequestMinSize)
				putU16(msg, 8, ^uint16(0))
				putU32(msg, 12, ^uint32(0)-uint32(^uint16(0))+2)
				return msg
			},
			read: func(t *testing.T, data []byte) {
				qir := QueryInfoRequest{Request: Request{data: data}}
				if qir.Validate(true) != nil {
					return
				}
				if buf := qir.InputBuffer(); len(buf) != 0 {
					t.Errorf("the field came back with %d bytes in it", len(buf))
				}
			},
		},
		{
			name: "query directory file name",
			build: func() []byte {
				msg := message(SMB2_QUERY_DIRECTORY, SMB2QueryDirectoryRequestStructureSize, SMB2QueryDirectoryRequestMinSize)
				putU16(msg, 24, ^uint16(0))
				putU16(msg, 26, 2)
				return msg
			},
			read: func(t *testing.T, data []byte) {
				qdr := QueryDirectoryRequest{Request: Request{data: data}}
				if qdr.Validate(true) != nil {
					return
				}
				if name := qdr.FileName(); name != "" {
					t.Errorf("the field came back as %q", name)
				}
			},
		},
		{
			name: "ioctl input buffer",
			build: func() []byte {
				msg := message(SMB2_IOCTL, SMB2IoctlRequestStructureSize, SMB2IoctlRequestMinSize)
				putU32(msg, 24, ^uint32(0)&^7)
				putU32(msg, 28, 16)
				return msg
			},
			read: func(t *testing.T, data []byte) {
				ir := IoctlRequest{Request: Request{data: data}}
				if ir.Validate(true) != nil {
					return
				}
				if buf := ir.InputBuffer(); len(buf) != 0 {
					t.Errorf("the field came back with %d bytes in it", len(buf))
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.read(t, tt.build())
		})
	}
}

// FuzzGetRequests keeps the parsing on its feet whatever arrives. Everything it reads is a
// message a peer sent, and the whole of it is looked at before anybody has authenticated, so a
// message that panics is a message that stops the server.
func FuzzGetRequests(f *testing.F) {
	f.Add(header(0))
	f.Add(header(SMB2HeaderSize))
	f.Add(append(header(SMB2HeaderSize), header(0)...))
	f.Add(header(0x30303030))
	f.Add([]byte{0xfe, 'S', 'M', 'B'})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		reqs, err := GetRequests(data, 0, 0, false)
		if err != nil {
			return
		}

		// Each request is taken the way the server takes it: cast to the command its header
		// claims, checked by the rules of that command, and only then read. Reading one that has
		// not passed its own check would be asking the accessors to answer for bytes the server
		// would never have handed them.
		for _, req := range reqs {
			if req == nil {
				continue
			}

			switch req.Header().Command() {
			case SMB2_CREATE:
				cr := CreateRequest{Request: *req}
				if cr.Validate(true) != nil {
					continue
				}
				_ = cr.DesiredAccess()
				_ = cr.CreateOptions()
				_ = cr.Filename()
				_, _ = cr.CreateContexts()

			case SMB2_QUERY_INFO:
				qir := QueryInfoRequest{Request: *req}
				if qir.Validate(true) != nil {
					continue
				}
				_ = qir.InputBuffer()
				_ = qir.FileID()
				_ = qir.OutputBufferLength()

			case SMB2_SESSION_SETUP:
				ssr := SessionSetupRequest{Request: *req}
				if ssr.Validate(true) != nil {
					continue
				}
				_ = ssr.SecurityBuffer()
				_ = ssr.Flags()
				_ = ssr.PreviousSessionID()

			case SMB2_WRITE:
				wr := WriteRequest{Request: *req}
				if wr.Validate(true) != nil {
					continue
				}
				_ = wr.FileID()
				_ = wr.Buffer()

			case SMB2_READ:
				rr := ReadRequest{Request: *req}
				if rr.Validate(true) != nil {
					continue
				}
				_ = rr.FileID()
				_ = rr.Length()

			case SMB2_SET_INFO:
				sir := SetInfoRequest{Request: *req}
				if sir.Validate(true) != nil {
					continue
				}
				_ = sir.Buffer()
				_ = sir.FileID()

			case SMB2_TREE_CONNECT:
				tcr := TreeConnectRequest{Request: *req}
				if tcr.Validate(true) != nil {
					continue
				}
				_ = tcr.PathName()
			}
		}
	})
}
