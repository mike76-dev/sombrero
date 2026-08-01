package main

import (
	"bytes"
	"crypto/aes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"testing"

	"github.com/mike76-dev/sombrero/internal/cmac"
	"github.com/mike76-dev/sombrero/internal/gmac"
	"github.com/mike76-dev/sombrero/smb2"
)

// signatureFor works out the signature that belongs on a message, by the method the dialect and
// the negotiated algorithm call for. It is the half of the protocol the peer holds, written out
// here so that the tests check the bytes on the wire rather than ask the server to agree with
// itself.
//
// fromServer says which side signed. Under GMAC the nonce carries a bit that tells the two
// directions apart, so the same bytes signed by the server and by the client do not come out with
// the same signature; the other methods sign a message the same way whichever way it is going.
func signatureFor(t *testing.T, msg, key []byte, dialect, algo uint16, fromServer bool) []byte {
	t.Helper()

	// The signature is worked out over the message with the field it will go in left empty, and
	// with the flag that says it is signed already set.
	msg = bytes.Clone(msg)
	smb2.Header(msg).SetFlag(smb2.FLAGS_SIGNED)
	smb2.Header(msg).WipeSignature()

	var signer hash.Hash
	switch {
	case dialect == smb2.SMB_DIALECT_202 || dialect == smb2.SMB_DIALECT_21:
		signer = hmac.New(sha256.New, key)

	case dialect == smb2.SMB_DIALECT_311 && algo == smb2.AES_GMAC:
		// The nonce is the message ID, and the low bits of the byte after it say which side signed
		// and whether the message is a cancel.
		nonce := make([]byte, 12)
		binary.LittleEndian.PutUint64(nonce[:8], smb2.Header(msg).MessageID())
		if fromServer {
			nonce[8] |= 1
		}
		if smb2.Header(msg).Command() == smb2.SMB2_CANCEL {
			nonce[8] |= 2
		}

		var err error
		signer, err = gmac.New(key, nonce)
		if err != nil {
			t.Fatalf("could not build the signer: %v", err)
		}

	default:
		ciph, err := aes.NewCipher(key)
		if err != nil {
			t.Fatalf("could not build the cipher: %v", err)
		}
		signer = cmac.New(ciph)
	}

	signer.Write(msg)

	return signer.Sum(nil)[:16]
}

// clientSignature is the signature a client puts on a request it sends.
func clientSignature(t *testing.T, msg, key []byte, dialect, algo uint16) []byte {
	t.Helper()

	return signatureFor(t, msg, key, dialect, algo, false)
}

// serverSignature is the signature the server puts on a response it sends.
func serverSignature(t *testing.T, msg, key []byte, dialect, algo uint16) []byte {
	t.Helper()

	return signatureFor(t, msg, key, dialect, algo, true)
}

// signed puts the signature of the client on a message, so that the server has one to check.
func signed(t *testing.T, msg, key []byte, dialect, algo uint16) []byte {
	t.Helper()

	smb2.Header(msg).SetFlag(smb2.FLAGS_SIGNED)
	smb2.Header(msg).SetSignature(clientSignature(t, msg, key, dialect, algo))

	return msg
}

// request parses a message into the request the server works with.
func request(t *testing.T, msg []byte) *smb2.Request {
	t.Helper()

	reqs, err := smb2.GetRequests(msg, 0, 0, false)
	if err != nil {
		t.Fatalf("the message did not parse as a request: %v", err)
	}

	return reqs[0]
}

// echoRequest builds the bytes of an SMB2_ECHO request, the smallest thing a client can send that
// is worth signing.
func echoRequest(mid, sid uint64, tid uint32) []byte {
	msg := make([]byte, smb2.SMB2HeaderSize+4)
	h := smb2.NewHeader(msg)
	h.SetCommand(smb2.SMB2_ECHO)
	h.SetMessageID(mid)
	h.SetSessionID(sid)
	h.SetTreeID(tid)
	h.SetCreditCharge(1)
	binary.LittleEndian.PutUint16(msg[smb2.SMB2HeaderSize:smb2.SMB2HeaderSize+2], 4)

	return msg
}

// sessionSetupRequest builds the bytes of an SMB2_SESSION_SETUP request carrying the given flags.
// A binding request is one that arrives with SESSION_FLAG_BINDING set.
func sessionSetupRequest(mid, sid uint64, flags uint8) []byte {
	msg := make([]byte, smb2.SMB2HeaderSize+smb2.SMB2SessionSetupRequestMinSize)
	h := smb2.NewHeader(msg)
	h.SetCommand(smb2.SMB2_SESSION_SETUP)
	h.SetMessageID(mid)
	h.SetSessionID(sid)
	h.SetCreditCharge(1)

	body := msg[smb2.SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[0:2], smb2.SMB2SessionSetupRequestStructureSize)
	body[2] = flags

	return msg
}

// keyed gives the channel of the connection a signing key of its own, told apart from the key of
// the session so that a test can see which of the two was reached for.
func (cl *testClient) keyed(b byte) []byte {
	key := bytes.Repeat([]byte{b}, 16)
	cl.ss.bindChannel(cl.conn, key)

	return key
}

// TestValidateRequestAcceptsASignedRequest is the request that arrives signed with the key of the
// channel it came in on.
func TestValidateRequestAcceptsASignedRequest(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	key := cl.keyed(0x11)

	req := request(t, signed(t, echoRequest(1, cl.ss.sessionID, cl.tc.treeID), key, smb2.SMB_DIALECT_311, 0))
	if err := cl.ss.validateRequest(req, cl.conn); err != nil {
		t.Fatalf("the server refused a correctly signed request: %v", err)
	}
}

// TestValidateRequestRefusesATamperedRequest is the request whose bytes changed on the way. The
// signature is over the whole message, so a change anywhere in it is a change to the signature.
func TestValidateRequestRefusesATamperedRequest(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	key := cl.keyed(0x11)

	msg := signed(t, echoRequest(1, cl.ss.sessionID, cl.tc.treeID), key, smb2.SMB_DIALECT_311, 0)
	msg[smb2.SMB2HeaderSize+2]++

	if err := cl.ss.validateRequest(request(t, msg), cl.conn); !errors.Is(err, errInvalidSignature) {
		t.Fatalf("the server answered %v, want it to refuse a request that changed on the way", err)
	}
}

// TestValidateRequestRefusesAnotherKeysSignature is the request signed with a key the channel
// does not hold — the signature is well formed, and belongs to somebody else.
func TestValidateRequestRefusesAnotherKeysSignature(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	cl.keyed(0x11)

	other := bytes.Repeat([]byte{0x22}, 16)
	req := request(t, signed(t, echoRequest(1, cl.ss.sessionID, cl.tc.treeID), other, smb2.SMB_DIALECT_311, 0))

	if err := cl.ss.validateRequest(req, cl.conn); !errors.Is(err, errInvalidSignature) {
		t.Fatalf("the server answered %v, want it to refuse a signature made with another key", err)
	}
}

// TestValidateRequestPassesOverAnUnsignedRequest is the request that never claimed to be signed.
// There is nothing to check; whether it was allowed to arrive unsigned is settled elsewhere.
func TestValidateRequestPassesOverAnUnsignedRequest(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	cl.keyed(0x11)

	req := request(t, echoRequest(1, cl.ss.sessionID, cl.tc.treeID))
	if err := cl.ss.validateRequest(req, cl.conn); err != nil {
		t.Fatalf("the server answered %v, want it to leave an unsigned request to the caller", err)
	}
}

// TestValidateRequestPassesOverAnEncryptedRequest is the request that arrived sealed. Coming
// apart under the key of the session is what stands in for a signature; it is not signed on top.
func TestValidateRequestPassesOverAnEncryptedRequest(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").encrypting()

	msg := echoRequest(1, cl.ss.sessionID, cl.tc.treeID)
	smb2.Header(msg).SetFlag(smb2.FLAGS_SIGNED)

	reqs, err := smb2.GetRequests(msg, 0, cl.ss.sessionID, false)
	if err != nil {
		t.Fatalf("the message did not parse as a request: %v", err)
	}

	if err := cl.ss.validateRequest(reqs[0], cl.conn); err != nil {
		t.Fatalf("the server answered %v, want it to leave an encrypted request alone", err)
	}
}

// TestValidateRequestWithoutAChannelKey is the signed request on a connection that is not bound
// to the session. There is no key to check it with, which is not the same as a bad signature: the
// caller turns the one into an error to the client and lets the other close the connection.
func TestValidateRequestWithoutAChannelKey(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	cl.ss.unbindChannel(cl.conn)

	req := request(t, signed(t, echoRequest(1, cl.ss.sessionID, cl.tc.treeID), cl.ss.signingKey, smb2.SMB_DIALECT_311, 0))
	if err := cl.ss.validateRequest(req, cl.conn); !errors.Is(err, errNoSigningKey) {
		t.Fatalf("the server answered %v, want it to say it has no key", err)
	}
}

// TestValidateBindingRequestUsesTheSessionKey is the request that binds a new channel to a
// session. It is signed with the key of the session, because the channel it is establishing has
// no key of its own until it has been through.
func TestValidateBindingRequestUsesTheSessionKey(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()

	// The connection is a channel of the session, and one that has not been given a key: exactly
	// where a binding request arrives.
	alt := cl.addChannel()

	msg := sessionSetupRequest(1, cl.ss.sessionID, smb2.SESSION_FLAG_BINDING)
	req := request(t, signed(t, msg, cl.ss.signingKey, smb2.SMB_DIALECT_311, 0))

	if err := cl.ss.validateRequest(req, alt.conn); err != nil {
		t.Fatalf("the server refused a binding request signed with the key of the session: %v", err)
	}
}

// TestValidateRequestUnderGMAC is the same check under the other signing algorithm 3.1.1 offers.
// The two are told apart by what the connection negotiated, and GMAC signs over a nonce that
// carries the message ID.
func TestValidateRequestUnderGMAC(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	cl.conn.signingAlgorithmID = smb2.AES_GMAC
	key := cl.keyed(0x11)

	req := request(t, signed(t, echoRequest(7, cl.ss.sessionID, cl.tc.treeID), key, smb2.SMB_DIALECT_311, smb2.AES_GMAC))
	if err := cl.ss.validateRequest(req, cl.conn); err != nil {
		t.Fatalf("the server refused a correctly signed request: %v", err)
	}

	// A signature that belongs to another message ID is not the one this message carries: the
	// nonce it was made under is part of it.
	other := request(t, signed(t, echoRequest(8, cl.ss.sessionID, cl.tc.treeID), key, smb2.SMB_DIALECT_311, smb2.AES_GMAC))
	other.Header().SetMessageID(7)

	if err := cl.ss.validateRequest(other, cl.conn); !errors.Is(err, errInvalidSignature) {
		t.Fatalf("the server answered %v, want it to refuse a signature made under another nonce", err)
	}
}

// TestValidateRequestOnAnOldDialect is the dialect family that has no channels. There is nothing
// to derive a key from, so the session key signs, and it signs with HMAC rather than a cipher.
func TestValidateRequestOnAnOldDialect(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing().speaking(smb2.SMB_DIALECT_21)

	// The channel holds a key of its own, to show it is not the one reached for.
	cl.keyed(0x11)

	req := request(t, signed(t, echoRequest(1, cl.ss.sessionID, cl.tc.treeID), cl.ss.sessionKey, smb2.SMB_DIALECT_21, 0))
	if err := cl.ss.validateRequest(req, cl.conn); err != nil {
		t.Fatalf("the server refused a request signed with the session key: %v", err)
	}
}

// TestSignUsesTheChannelKey is the response going out over a channel of the session. Every
// channel signs with its own key, so which connection it leaves by decides the signature.
func TestSignUsesTheChannelKey(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	key := cl.keyed(0x11)

	buf := echoRequest(1, cl.ss.sessionID, cl.tc.treeID)
	cl.ss.sign(buf, cl.conn)

	if !smb2.Header(buf).IsFlagSet(smb2.FLAGS_SIGNED) {
		t.Error("the response does not say it is signed")
	}
	if want := serverSignature(t, buf, key, smb2.SMB_DIALECT_311, 0); !bytes.Equal(smb2.Header(buf).Signature(), want) {
		t.Error("the response is not signed with the key of the channel it leaves by")
	}
}

// TestSignFallsBackToTheSessionKey is the response on a connection the session does not know as a
// channel. Nothing good can come of it, and what goes out is signed with the key of the session
// rather than left claiming a signature it does not carry.
func TestSignFallsBackToTheSessionKey(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	cl.ss.unbindChannel(cl.conn)

	buf := echoRequest(1, cl.ss.sessionID, cl.tc.treeID)
	cl.ss.sign(buf, cl.conn)

	if want := serverSignature(t, buf, cl.ss.signingKey, smb2.SMB_DIALECT_311, 0); !bytes.Equal(smb2.Header(buf).Signature(), want) {
		t.Error("the response is not signed with the key of the session")
	}
}

// TestSignSessionSetupFailureUsesTheSessionKey is the session setup that did not go through. It
// travels while the channel is still being established, so the client has nothing but the key of
// the session to check it with.
func TestSignSessionSetupFailureUsesTheSessionKey(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	channelKey := cl.keyed(0x11)

	buf := echoRequest(1, cl.ss.sessionID, cl.tc.treeID)
	smb2.Header(buf).SetCommand(smb2.SMB2_SESSION_SETUP)
	smb2.Header(buf).SetStatus(smb2.STATUS_MORE_PROCESSING_REQUIRED)
	cl.ss.sign(buf, cl.conn)

	if want := serverSignature(t, buf, cl.ss.signingKey, smb2.SMB_DIALECT_311, 0); !bytes.Equal(smb2.Header(buf).Signature(), want) {
		t.Error("the unfinished session setup is not signed with the key of the session")
	}
	if made := serverSignature(t, buf, channelKey, smb2.SMB_DIALECT_311, 0); bytes.Equal(smb2.Header(buf).Signature(), made) {
		t.Error("the unfinished session setup is signed with the key of the channel")
	}
}

// TestSignEveryResponseInAChain is the compound response. The client checks each of the responses
// in it on its own, so each carries a signature over its own bytes rather than the chain carrying
// one over all of them.
func TestSignEveryResponseInAChain(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	key := cl.keyed(0x11)

	first := echoRequest(1, cl.ss.sessionID, cl.tc.treeID)
	first = append(first, make([]byte, 8-len(first)%8)...) // The next response starts at a boundary.
	second := echoRequest(2, cl.ss.sessionID, cl.tc.treeID)

	smb2.Header(first).SetNextCommand(uint32(len(first)))
	buf := append(bytes.Clone(first), second...)

	cl.ss.sign(buf, cl.conn)

	for i, segment := range [][]byte{buf[:len(first)], buf[len(first):]} {
		if !smb2.Header(segment).IsFlagSet(smb2.FLAGS_SIGNED) {
			t.Errorf("response %d does not say it is signed", i)
		}
		if want := serverSignature(t, segment, key, smb2.SMB_DIALECT_311, 0); !bytes.Equal(smb2.Header(segment).Signature(), want) {
			t.Errorf("response %d is not signed over its own bytes", i)
		}
	}
}

// TestSignOnAnOldDialect is the dialect family that signs with the session key under HMAC.
func TestSignOnAnOldDialect(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing().speaking(smb2.SMB_DIALECT_21)
	cl.keyed(0x11)

	buf := echoRequest(1, cl.ss.sessionID, cl.tc.treeID)
	cl.ss.sign(buf, cl.conn)

	if want := serverSignature(t, buf, cl.ss.sessionKey, smb2.SMB_DIALECT_21, 0); !bytes.Equal(smb2.Header(buf).Signature(), want) {
		t.Error("the response is not signed with the session key")
	}
}

// TestSignRoundTrip is the two halves meeting, over every dialect and every algorithm the server
// signs under: what it signs is what it would accept, and what the client made of the same bytes
// is the same signature.
func TestSignRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		name    string
		dialect uint16
		algo    uint16
	}{
		{"2.0.2", smb2.SMB_DIALECT_202, 0},
		{"2.1", smb2.SMB_DIALECT_21, 0},
		{"3.0", smb2.SMB_DIALECT_30, 0},
		{"3.0.2", smb2.SMB_DIALECT_302, 0},
		{"3.1.1/unnegotiated", smb2.SMB_DIALECT_311, 0},
		{"3.1.1/CMAC", smb2.SMB_DIALECT_311, smb2.AES_CMAC},
		{"3.1.1/GMAC", smb2.SMB_DIALECT_311, smb2.AES_GMAC},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newSMBTest(t)
			cl := h.dial("alice").signing().speaking(tt.dialect)
			cl.conn.signingAlgorithmID = tt.algo

			// Which key signs is the business of the dialect: the ones with channels take the key
			// of the channel, the older ones the key of the session.
			key := cl.keyed(0x11)
			if !smb2.Is3X(tt.dialect) {
				key = cl.ss.sessionKey
			}

			buf := echoRequest(1, cl.ss.sessionID, cl.tc.treeID)
			cl.ss.sign(buf, cl.conn)

			if want := serverSignature(t, buf, key, tt.dialect, tt.algo); !bytes.Equal(smb2.Header(buf).Signature(), want) {
				t.Error("the response does not carry the signature the client would work it out to be")
			}

			// GMAC signs a response and a request differently on purpose: the nonce says which
			// side made it, so a response cannot be replayed back as a request that checks out.
			// Everything else signs a message by its bytes alone, and the server accepts its own.
			err := cl.ss.validateRequest(request(t, buf), cl.conn)
			if tt.algo == smb2.AES_GMAC {
				if !errors.Is(err, errInvalidSignature) {
					t.Fatalf("the server answered %v, want it to refuse its own response as a request", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("the server would not accept what it signed itself: %v", err)
			}
		})
	}
}

// clientSeal encrypts a message the way the client does: under the key the server opens incoming
// traffic with. The two directions have keys of their own, so what the server sends cannot be
// opened with the key it reads by.
func clientSeal(t *testing.T, ss *session, c *connection, msg []byte) []byte {
	t.Helper()

	sealer, err := ss.aead(ss.decryptionKey, c)
	if err != nil {
		t.Fatalf("could not build the cipher: %v", err)
	}

	nonce := make([]byte, sealer.NonceSize())
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}

	out := smb2.Header(make([]byte, smb2.SMB2TransformHeaderSize+len(msg)+16))
	out.SetProtocolID(smb2.PROTOCOL_SMB2_ENCRYPTED)
	out.SetNonce(nonce)
	out.SetOriginalMessageSize(uint32(len(msg)))
	out.SetEncryptionFlags(1)
	out.SetTransformSessionID(ss.sessionID)
	sealer.Seal(out[:smb2.SMB2TransformHeaderSize], nonce, msg, out.AssociatedData())
	out.SetEncryptionSignature(out[len(out)-16:])

	return out[:len(out)-16]
}

// TestEncryptRoundTrip is the message the server seals, opened with the key the client holds for
// that direction.
func TestEncryptRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		name    string
		dialect uint16
		cipher  uint16
	}{
		{"3.1.1/GCM", smb2.SMB_DIALECT_311, smb2.AES_128_GCM},
		{"3.1.1/CCM", smb2.SMB_DIALECT_311, smb2.AES_128_CCM},
		{"3.0.2", smb2.SMB_DIALECT_302, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newSMBTest(t)
			cl := h.dial("alice").speaking(tt.dialect).encrypting()
			cl.conn.cipherID = tt.cipher

			msg := echoRequest(1, cl.ss.sessionID, cl.tc.treeID)
			if got := cl.decrypted(cl.ss.encrypt(bytes.Clone(msg), cl.conn)); !bytes.Equal(got, msg) {
				t.Fatal("what came back is not what went in")
			}
		})
	}
}

// TestDecryptRoundTrip is the other direction: what the client sealed, opened by the server.
func TestDecryptRoundTrip(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").encrypting()

	msg := echoRequest(1, cl.ss.sessionID, cl.tc.treeID)
	sealed := clientSeal(t, cl.ss, cl.conn, bytes.Clone(msg))

	if got := cl.ss.decrypt(sealed, cl.conn); !bytes.Equal(got, msg) {
		t.Fatal("what came back is not what went in")
	}
}

// TestDecryptRefusesATamperedMessage is the sealed message that changed on the way. Nothing comes
// back out of it: the tag is over the ciphertext and over the header that travels in the clear.
func TestDecryptRefusesATamperedMessage(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").encrypting()

	for _, tt := range []struct {
		name string
		at   int
	}{
		{"in the sealed message", smb2.SMB2TransformHeaderSize},
		{"in the header it travels under", 36},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sealed := clientSeal(t, cl.ss, cl.conn, echoRequest(1, cl.ss.sessionID, cl.tc.treeID))
			sealed[tt.at]++

			if got := cl.ss.decrypt(sealed, cl.conn); got != nil {
				t.Fatal("the server opened a message that changed on the way")
			}
		})
	}
}

// TestDecryptWithoutACipher is the session that never settled on one. There is nothing to open
// the message with, and nothing comes back.
func TestDecryptWithoutACipher(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").encrypting()

	sealed := clientSeal(t, cl.ss, cl.conn, echoRequest(1, cl.ss.sessionID, cl.tc.treeID))
	cl.conn.cipherID = 0

	if got := cl.ss.decrypt(sealed, cl.conn); got != nil {
		t.Fatal("the server opened a message although no cipher was negotiated")
	}
}
