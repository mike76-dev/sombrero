package ntlm

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/mike76-dev/sombrero/spnego"
	"github.com/mike76-dev/sombrero/utils"
)

// negotiateMessage builds the message a client opens the exchange with.
func negotiateMessage(flags uint32) []byte {
	nmsg := make([]byte, 32)
	copy(nmsg[:8], signature)
	binary.LittleEndian.PutUint32(nmsg[8:12], NtLmNegotiate)
	binary.LittleEndian.PutUint32(nmsg[12:16], flags)

	return nmsg
}

// challenged returns a server that has answered a negotiate, which is the state it is in when an
// authenticate arrives in the ordinary run of things.
func challenged(t *testing.T) *Server {
	t.Helper()

	srv := NewServer("SOMBRERO", "WORKGROUP", knownAccount())
	if _, err := srv.Challenge(negotiateMessage(defaultFlags)); err != nil {
		t.Fatalf("the challenge would not go out: %v", err)
	}

	return srv
}

// authenticateMessage builds an AUTHENTICATE message carrying an NT response of the given length,
// with the user name and workgroup of the account behind it. What the response holds is not the
// point — everything read out of it is read before anything is checked.
func authenticateMessage(ntResponse []byte, flags uint32) []byte {
	userBytes := utils.EncodeStringToBytes(testUser)
	domainBytes := utils.EncodeStringToBytes(testWorkgroup)

	const hdr = 64
	amsg := make([]byte, hdr+len(ntResponse)+len(domainBytes)+len(userBytes))
	copy(amsg[:8], signature)
	binary.LittleEndian.PutUint32(amsg[8:12], NtLmAuthenticate)
	binary.LittleEndian.PutUint32(amsg[60:64], flags)

	off := hdr
	put := func(field int, b []byte) {
		binary.LittleEndian.PutUint16(amsg[field:field+2], uint16(len(b)))
		binary.LittleEndian.PutUint16(amsg[field+2:field+4], uint16(len(b)))
		binary.LittleEndian.PutUint32(amsg[field+4:field+8], uint32(off))
		copy(amsg[off:], b)
		off += len(b)
	}
	put(20, ntResponse)
	put(28, domainBytes)
	put(36, userBytes)

	return amsg
}

// TestAuthenticateRefusesAResponseCutShort is the message that took the server off the machine.
// Everything read out of the NT response — the timestamp, the challenge the client chose, the
// target information — is read before a single thing about the message has been checked, and the
// length of it is the client's to choose. A response of anything under the fixed size reached
// past its own end and ended the process; nothing in the read path recovers, so it is not the
// connection that goes but every session on the server.
//
// Getting here needs a user name that exists and nothing else. A user name is not a secret.
func TestAuthenticateRefusesAResponseCutShort(t *testing.T) {
	for n := 0; n < ntlmv2ResponseMinSize; n++ {
		srv := challenged(t)

		err := srv.Authenticate(authenticateMessage(make([]byte, n), defaultFlags&^NTLMSSP_NEGOTIATE_KEY_EXCH))
		if err == nil {
			t.Errorf("an NT response of %d bytes was accepted", n)
		}
		if srv.Session() != nil {
			t.Fatalf("an NT response of %d bytes left a session behind", n)
		}
	}
}

// TestAuthenticateRefusesAResponseOfTheRightLengthButWrongContents is the boundary from the other
// side: a response long enough to be read is read, and then turned away for what it says rather
// than for how long it is.
func TestAuthenticateRefusesAResponseOfTheRightLengthButWrongContents(t *testing.T) {
	srv := challenged(t)

	err := srv.Authenticate(authenticateMessage(make([]byte, ntlmv2ResponseMinSize), defaultFlags&^NTLMSSP_NEGOTIATE_KEY_EXCH))
	if err == nil {
		t.Fatal("a response of nothing but zeroes authenticated")
	}
	if srv.Session() != nil {
		t.Error("a response that did not check out left a session behind")
	}
}

// TestAuthenticateRefusesWhatArrivesBeforeAChallenge is the exchange run out of order. The server
// compares the response against the challenge it sent, and a connection that was never asked for
// one has nothing to compare against: it read the challenge out of a buffer that was never filled
// in and ended the process.
//
// A client picks which leg it is sending — the server tells them apart by looking at the message
// — so an authenticate arriving first is a client's to arrange, and session binding is a path
// that reaches this with a connection that has only ever negotiated.
func TestAuthenticateRefusesWhatArrivesBeforeAChallenge(t *testing.T) {
	srv := NewServer("SOMBRERO", "WORKGROUP", knownAccount())

	err := srv.Authenticate(authenticateMessage(make([]byte, ntlmv2ResponseMinSize), defaultFlags&^NTLMSSP_NEGOTIATE_KEY_EXCH))
	if err == nil {
		t.Fatal("an authenticate was taken by a server that had issued no challenge")
	}
	if srv.Session() != nil {
		t.Error("an authenticate with no challenge behind it left a session behind")
	}
}

// TestAuthenticateRefusesAMalformedMessage walks the shapes an AUTHENTICATE message arrives in
// when it is not one. Every one of these is read before the client has proved anything.
func TestAuthenticateRefusesAMalformedMessage(t *testing.T) {
	good := authenticateMessage(make([]byte, ntlmv2ResponseMinSize), defaultFlags&^NTLMSSP_NEGOTIATE_KEY_EXCH)

	// A field whose offset points past the end of what arrived.
	past := bytes.Clone(good)
	binary.LittleEndian.PutUint32(past[24:28], 0xfffffff0)

	// A field whose length runs past the end from an offset inside it.
	overlong := bytes.Clone(good)
	binary.LittleEndian.PutUint16(overlong[20:22], 0xffff)
	binary.LittleEndian.PutUint16(overlong[22:24], 0xffff)

	// A maximum length smaller than the length it bounds.
	inverted := bytes.Clone(good)
	binary.LittleEndian.PutUint16(inverted[22:24], 1)

	for _, tt := range []struct {
		name string
		msg  []byte
	}{
		{"nothing at all", nil},
		{"a single byte", []byte{0}},
		{"the signature and no more", signature},
		{"a header cut short", make([]byte, 63)},
		{"a header of the right length and nothing in it", make([]byte, 64)},
		{"the wrong message type", func() []byte {
			m := bytes.Clone(good)
			binary.LittleEndian.PutUint32(m[8:12], NtLmNegotiate)
			return m
		}()},
		{"a signature that is not the one", func() []byte {
			m := bytes.Clone(good)
			m[0] ^= 1
			return m
		}()},
		{"an offset past the end", past},
		{"a length running past the end", overlong},
		{"a maximum length under the length", inverted},
		{"nothing but ones", bytes.Repeat([]byte{0xff}, 128)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := challenged(t)

			if err := srv.Authenticate(tt.msg); err == nil {
				t.Fatal("a message that is not an authenticate was taken as one")
			}
			if srv.Session() != nil {
				t.Error("a message that was refused left a session behind")
			}
		})
	}
}

// TestChallengeRefusesAMalformedNegotiate is the first message of the exchange, which arrives
// before anything at all is known about the client.
func TestChallengeRefusesAMalformedNegotiate(t *testing.T) {
	for _, tt := range []struct {
		name string
		msg  []byte
	}{
		{"nothing at all", nil},
		{"a single byte", []byte{0}},
		{"a message cut short", make([]byte, 31)},
		{"nothing in it", make([]byte, 32)},
		{"a signature that is not the one", func() []byte {
			m := negotiateMessage(defaultFlags)
			m[0] ^= 1
			return m
		}()},
		{"the wrong message type", func() []byte {
			m := negotiateMessage(defaultFlags)
			binary.LittleEndian.PutUint32(m[8:12], NtLmAuthenticate)
			return m
		}()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewServer("SOMBRERO", "WORKGROUP", knownAccount())

			if _, err := srv.Challenge(tt.msg); err == nil {
				t.Fatal("a message that is not a negotiate was answered with a challenge")
			}
		})
	}
}

// authenticateExchangingKey drives a whole exchange that checks out, and asks for the session key
// to be exchanged, carrying one of the given length. Everything up to the key is correct, so the
// message gets past the comparison and reaches the key handling behind it.
func authenticateExchangingKey(t *testing.T, srv *Server, keyLen int) error {
	t.Helper()

	cmsg, err := srv.Challenge(negotiateMessage(defaultFlags))
	if err != nil {
		t.Fatalf("the challenge would not go out: %v", err)
	}

	serverChallenge := cmsg[24:32]
	tiOff := binary.LittleEndian.Uint32(cmsg[44:48])
	tiLen := binary.LittleEndian.Uint16(cmsg[40:42])
	targetInfo := cmsg[tiOff : tiOff+uint32(tiLen)]

	userBytes := utils.EncodeStringToBytes(testUser)
	domainBytes := utils.EncodeStringToBytes(testWorkgroup)

	key := ntowfv2Hash(utils.EncodeStringToBytes(strings.ToUpper(testUser)), ntHashOf(testPassword), domainBytes)
	ntResp := make([]byte, 16+28+len(targetInfo))
	encodeNtlmv2Response(ntResp, hmac.New(md5.New, key), serverChallenge, make([]byte, 8), make([]byte, 8), bytesEncoder(targetInfo))

	sessionKey := make([]byte, keyLen)

	const hdr = 64
	amsg := make([]byte, hdr+len(ntResp)+len(domainBytes)+len(userBytes)+len(sessionKey))
	copy(amsg[:8], signature)
	binary.LittleEndian.PutUint32(amsg[8:12], NtLmAuthenticate)
	binary.LittleEndian.PutUint32(amsg[60:64], (defaultFlags&^NTLMSSP_NEGOTIATE_VERSION)|NTLMSSP_NEGOTIATE_KEY_EXCH)

	off := hdr
	put := func(field int, b []byte) {
		binary.LittleEndian.PutUint16(amsg[field:field+2], uint16(len(b)))
		binary.LittleEndian.PutUint16(amsg[field+2:field+4], uint16(len(b)))
		binary.LittleEndian.PutUint32(amsg[field+4:field+8], uint32(off))
		copy(amsg[off:], b)
		off += len(b)
	}
	put(20, ntResp)      // NtChallengeResponseFields
	put(28, domainBytes) // DomainNameFields
	put(36, userBytes)   // UserNameFields
	put(52, sessionKey)  // EncryptedRandomSessionKeyFields

	return srv.Authenticate(amsg)
}

// TestAuthenticateRefusesASessionKeyOfTheWrongLength is the key the client sends for the server to
// unwrap, which it says the length of. The key it is unwrapped into is sixteen bytes: a longer one
// takes the stream cipher past the end of what it was given, and a shorter one leaves the rest of
// the key as the zeroes it was made with — a key neither side agreed on, and one an attacker gets
// to choose the length of.
//
// Everything else in these messages checks out, so each reaches the key handling rather than being
// turned away before it.
func TestAuthenticateRefusesASessionKeyOfTheWrongLength(t *testing.T) {
	for _, keyLen := range []int{0, 1, 8, 15, 17, 32, 255} {
		srv := NewServer("SOMBRERO", "WORKGROUP", knownAccount())

		if err := authenticateExchangingKey(t, srv, keyLen); err == nil {
			t.Errorf("a session key of %d bytes was taken", keyLen)
		}
		if srv.Session() != nil {
			t.Errorf("a session key of %d bytes left a session behind", keyLen)
		}
	}
}

// TestAuthenticateTakesASessionKeyOfTheRightLength is the boundary from the other side, and the
// control for the case above: without it the refusals there would be satisfied by a message this
// test simply builds wrong.
func TestAuthenticateTakesASessionKeyOfTheRightLength(t *testing.T) {
	srv := NewServer("SOMBRERO", "WORKGROUP", knownAccount())

	if err := authenticateExchangingKey(t, srv, 16); err != nil {
		t.Fatalf("a session key of the right length was turned away: %v", err)
	}
	if got := len(srv.Session().SessionKey()); got != 16 {
		t.Errorf("the key that came out is %d bytes long, want 16", got)
	}
}

// TestNegotiateOffersNTLM is the token that goes out in the negotiate response, before any of the
// exchange has begun. It is what tells a client which mechanism to authenticate with, so the one
// mechanism this server has must be named in it.
func TestNegotiateOffersNTLM(t *testing.T) {
	srv := NewServer("SOMBRERO", "WORKGROUP", knownAccount())

	token, err := srv.Negotiate()
	if err != nil {
		t.Fatalf("the token would not go out: %v", err)
	}

	init, err := spnego.DecodeNegTokenInit2(token)
	if err != nil {
		t.Fatalf("the token the server sends does not decode: %v", err)
	}

	if len(init.MechTypes) != 1 || !init.MechTypes[0].Equal(spnego.NlmpOid) {
		t.Errorf("the mechanisms offered came out %v, want just NTLM", init.MechTypes)
	}
}

// TestChallengeIsWellFormed reads back the message the server sends, since a client parses it the
// same way and nothing else in this package checks that it lays out as it says it does.
func TestChallengeIsWellFormed(t *testing.T) {
	srv := NewServer("SOMBRERO", "WORKGROUP", knownAccount())

	cmsg, err := srv.Challenge(negotiateMessage(defaultFlags))
	if err != nil {
		t.Fatalf("the challenge would not go out: %v", err)
	}

	if !bytes.Equal(cmsg[:8], signature) {
		t.Error("the challenge does not open with the signature")
	}
	if got := binary.LittleEndian.Uint32(cmsg[8:12]); got != NtLmChallenge {
		t.Errorf("the message type is %d, want %d", got, NtLmChallenge)
	}

	// The target name and the target information both say where they are and how long they are,
	// and both have to fall inside the message a client is handed.
	nameLen := binary.LittleEndian.Uint16(cmsg[12:14])
	nameOff := binary.LittleEndian.Uint32(cmsg[16:20])
	if int(nameOff)+int(nameLen) > len(cmsg) {
		t.Errorf("the target name runs to %d in a message of %d", int(nameOff)+int(nameLen), len(cmsg))
	}
	if got := utils.DecodeToString(cmsg[nameOff : nameOff+uint32(nameLen)]); got != "SOMBRERO" {
		t.Errorf("the target name came out %q, want %q", got, "SOMBRERO")
	}

	infoLen := binary.LittleEndian.Uint16(cmsg[40:42])
	infoOff := binary.LittleEndian.Uint32(cmsg[44:48])
	if int(infoOff)+int(infoLen) > len(cmsg) {
		t.Fatalf("the target information runs to %d in a message of %d", int(infoOff)+int(infoLen), len(cmsg))
	}

	// The information is a list of pairs ending in one that marks the end, and a client walks it
	// exactly as parseAvPairs does here.
	pairs, ok := parseAvPairs(cmsg[infoOff : infoOff+uint32(infoLen)])
	if !ok {
		t.Fatal("the target information the server sends does not parse")
	}
	for _, id := range []uint16{MsvAvNbComputerName, MsvAvNbDomainName, MsvAvDnsComputerName, MsvAvDnsDomainName, MsvAvTimestamp} {
		if _, found := pairs[id]; !found {
			t.Errorf("the target information does not carry the pair %d", id)
		}
	}
	if got := len(pairs[MsvAvTimestamp]); got != 8 {
		t.Errorf("the timestamp is %d bytes long, want 8", got)
	}
}

// TestChallengeIsDifferentEveryTime is the one thing the challenge has to be. It is what stops a
// response captured off the wire from being sent again, so two exchanges must never be given the
// same one to answer.
func TestChallengeIsDifferentEveryTime(t *testing.T) {
	seen := make(map[string]struct{})

	for i := 0; i < 64; i++ {
		srv := NewServer("SOMBRERO", "WORKGROUP", knownAccount())
		cmsg, err := srv.Challenge(negotiateMessage(defaultFlags))
		if err != nil {
			t.Fatalf("the challenge would not go out: %v", err)
		}

		challenge := string(cmsg[24:32])
		if _, found := seen[challenge]; found {
			t.Fatalf("the same challenge was issued twice after %d exchanges", i)
		}
		seen[challenge] = struct{}{}

		if challenge == string(make([]byte, 8)) {
			t.Fatal("the challenge issued was nothing but zeroes")
		}
	}
}

// TestIsAuthenticate is what the server tells the two legs of a binding apart by, so it has to be
// answered for a message of any shape.
func TestIsAuthenticate(t *testing.T) {
	for _, tt := range []struct {
		name string
		msg  []byte
		want bool
	}{
		{"an authenticate", authenticateMessage(make([]byte, 44), defaultFlags), true},
		{"a negotiate", negotiateMessage(defaultFlags), false},
		{"nothing at all", nil, false},
		{"a single byte", []byte{0}, false},
		{"the signature and no more", signature, false},
		{"eleven bytes", make([]byte, 11), false},
		{"a signature that is not the one", func() []byte {
			m := authenticateMessage(make([]byte, 44), defaultFlags)
			m[0] ^= 1
			return m
		}(), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAuthenticate(tt.msg); got != tt.want {
				t.Errorf("it said %v, want %v", got, tt.want)
			}
		})
	}
}

// TestParseAvPairs walks the list of pairs a client sends inside its response. It is read out of
// the message before the message has been checked, so every shape of it has to be answered.
func TestParseAvPairs(t *testing.T) {
	pair := func(id uint16, value []byte) []byte {
		var buf []byte
		buf = binary.LittleEndian.AppendUint16(buf, id)
		buf = binary.LittleEndian.AppendUint16(buf, uint16(len(value)))
		return append(buf, value...)
	}
	eol := []byte{0, 0, 0, 0}

	t.Run("a list as a client sends it", func(t *testing.T) {
		list := append(pair(MsvAvNbComputerName, utils.EncodeStringToBytes("HOST")), eol...)

		pairs, ok := parseAvPairs(list)
		if !ok {
			t.Fatal("a well-formed list would not parse")
		}
		if got := utils.DecodeToString(pairs[MsvAvNbComputerName]); got != "HOST" {
			t.Errorf("the name came out %q, want %q", got, "HOST")
		}
	})

	for _, tt := range []struct {
		name string
		list []byte
	}{
		{"nothing at all", nil},
		{"under a single pair", []byte{0, 0, 0}},
		{"no end marker", pair(MsvAvNbComputerName, []byte("AB"))},
		{"a length running past the end", append(pair(MsvAvNbComputerName, nil)[:2], 0xff, 0xff)},
		{"a pair cut short", append(append([]byte{}, pair(MsvAvFlags, []byte{1, 2, 3, 4})...), 0, 0)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := parseAvPairs(tt.list); ok {
				t.Error("a list that is not one was parsed all the same")
			}
		})
	}
}

// FuzzAuthenticate walks the bytes of the second leg of a session setup. It arrives from a client
// that has proved nothing, and everything the decoder reads out of it is a length the client
// chose. The property is that an answer comes back at all: a panic here is the process, and with
// it every session on the server.
func FuzzAuthenticate(f *testing.F) {
	f.Add(authenticateMessage(make([]byte, ntlmv2ResponseMinSize), defaultFlags))
	f.Add(authenticateMessage(make([]byte, 0), defaultFlags))
	f.Add(authenticateMessage(make([]byte, 16), defaultFlags&^NTLMSSP_NEGOTIATE_KEY_EXCH))
	f.Add(negotiateMessage(defaultFlags))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, msg []byte) {
		srv := NewServer("SOMBRERO", "WORKGROUP", knownAccount())
		if _, err := srv.Challenge(negotiateMessage(defaultFlags)); err != nil {
			t.Skip()
		}

		// A message that is turned away must leave nothing behind: the caller reads the session
		// off the server as soon as this comes back without an error.
		if err := srv.Authenticate(msg); err != nil {
			if srv.Session() != nil {
				t.Fatal("a message that was refused left a session behind")
			}
		}
	})
}

// FuzzChallenge walks the first message of the exchange, which is the earliest thing a client
// sends that this package reads.
func FuzzChallenge(f *testing.F) {
	f.Add(negotiateMessage(defaultFlags))
	f.Add(negotiateMessage(0))
	f.Add(negotiateMessage(0xffffffff))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, msg []byte) {
		srv := NewServer("SOMBRERO", "WORKGROUP", knownAccount())

		cmsg, err := srv.Challenge(msg)
		if err != nil {
			return
		}

		// A challenge that went out is one a client will parse, so what it says about itself has
		// to hold: every field has to fall inside the message carrying it.
		if len(cmsg) < 48 {
			t.Fatalf("a challenge of %d bytes went out", len(cmsg))
		}

		nameLen := binary.LittleEndian.Uint16(cmsg[12:14])
		nameOff := binary.LittleEndian.Uint32(cmsg[16:20])
		if int(nameOff)+int(nameLen) > len(cmsg) {
			t.Fatalf("the target name runs to %d in a message of %d", int(nameOff)+int(nameLen), len(cmsg))
		}

		infoLen := binary.LittleEndian.Uint16(cmsg[40:42])
		infoOff := binary.LittleEndian.Uint32(cmsg[44:48])
		if int(infoOff)+int(infoLen) > len(cmsg) {
			t.Fatalf("the target information runs to %d in a message of %d", int(infoOff)+int(infoLen), len(cmsg))
		}
	})
}
