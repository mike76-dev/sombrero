package smb2

import (
	"encoding/binary"
	"testing"
)

// createMessage builds a CREATE request with room for a chain of create contexts at the
// given offset, and the chain's length filled in.
func createMessage(contextsAt, contextsLen int) []byte {
	msg := message(SMB2_CREATE, SMB2CreateRequestStructureSize, SMB2CreateRequestMinSize)
	msg = append(msg, make([]byte, contextsAt+contextsLen)...)
	putU32(msg, 48, uint32(SMB2HeaderSize+contextsAt))
	putU32(msg, 52, uint32(contextsLen))

	return msg
}

// createContext writes an empty create context of the given name, with the step to the
// next one, at the given offset behind the fixed part of the request.
func createContext(msg []byte, at int, next uint32, name string) {
	at += SMB2HeaderSize
	binary.LittleEndian.PutUint32(msg[at:], next)
	binary.LittleEndian.PutUint16(msg[at+4:], 16)
	binary.LittleEndian.PutUint16(msg[at+6:], 4)
	copy(msg[at+16:], name)
}

// TestCreateContextsFollowAWellFormedChain is the control: two contexts a proper step
// apart both come back.
func TestCreateContextsFollowAWellFormedChain(t *testing.T) {
	msg := createMessage(64, 48)
	createContext(msg, 64, 24, "DHnQ")
	createContext(msg, 88, 0, "MxAc")

	cr := CreateRequest{Request: Request{data: msg}}
	if err := cr.Validate(true); err != nil {
		t.Fatalf("a well-formed request was refused: %v", err)
	}
	ctxs, err := cr.CreateContexts()
	if err != nil {
		t.Fatalf("a well-formed chain was refused: %v", err)
	}
	if len(ctxs) != 2 {
		t.Fatalf("%d contexts came back, want 2", len(ctxs))
	}
}

// TestCreateContextsRefuseAStepBackIntoTheContext is a Next that is shorter than the fixed
// part of the context, which has the walk reading the next header out of the bytes of the
// context it just read.
func TestCreateContextsRefuseAStepBackIntoTheContext(t *testing.T) {
	for _, tt := range []struct {
		name string
		next uint32
	}{
		{"one byte", 1},
		{"short of the fixed part", 8},
		{"one short of the fixed part", 15},
	} {
		t.Run(tt.name, func(t *testing.T) {
			msg := createMessage(64, 48)
			createContext(msg, 64, tt.next, "DHnQ")

			cr := CreateRequest{Request: Request{data: msg}}
			if _, err := cr.CreateContexts(); err == nil {
				t.Fatal("a chain stepping back into its own context was accepted")
			}
		})
	}
}
