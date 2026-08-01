package spnego

import (
	"bytes"
	"encoding/asn1"
	"testing"
)

// ntlmOid and the tokens below stand in for what a client actually offers during session setup.
var mechTypes = []asn1.ObjectIdentifier{NlmpOid}

// TestNegTokenInitRoundTrip is the first leg of a session setup written out and read back. What
// the server encodes is what a client parses, and what a client encodes is what the server parses
// at conn.go, so the two directions have to describe the same token.
func TestNegTokenInitRoundTrip(t *testing.T) {
	token := []byte("the NTLM negotiate message")

	bs, err := EncodeNegTokenInit(mechTypes, token)
	if err != nil {
		t.Fatalf("could not write the token: %v", err)
	}

	// [APPLICATION 0], which is what a SPNEGO token opens with and what the reader is told to
	// expect. It is patched in after the fact, so it is worth pinning here.
	if bs[0] != 0x60 {
		t.Errorf("the token opens with %#02x, want 0x60", bs[0])
	}

	init, err := DecodeNegTokenInit(bs)
	if err != nil {
		t.Fatalf("could not read back what was written: %v", err)
	}

	if len(init.MechTypes) != 1 || !init.MechTypes[0].Equal(NlmpOid) {
		t.Errorf("the mechanisms came back %v, want just NTLM", init.MechTypes)
	}
	if !bytes.Equal(init.MechToken, token) {
		t.Errorf("the inner token came back %q, want %q", init.MechToken, token)
	}
}

// TestNegTokenInit2RoundTrip is the same for the token the server puts in its negotiate response,
// which carries the hint rather than an inner token.
func TestNegTokenInit2RoundTrip(t *testing.T) {
	bs, err := EncodeNegTokenInit2(mechTypes)
	if err != nil {
		t.Fatalf("could not write the token: %v", err)
	}

	if bs[0] != 0x60 {
		t.Errorf("the token opens with %#02x, want 0x60", bs[0])
	}

	init, err := DecodeNegTokenInit2(bs)
	if err != nil {
		t.Fatalf("could not read back what was written: %v", err)
	}

	if len(init.MechTypes) != 1 || !init.MechTypes[0].Equal(NlmpOid) {
		t.Errorf("the mechanisms came back %v, want just NTLM", init.MechTypes)
	}
}

// TestNegTokenInit2CarriesTheHint is the string every implementation puts in this token and no
// implementation reads. It is there because it is expected to be there, so it is checked to have
// survived the encoding rather than been dropped as the empty optional field it looks like.
func TestNegTokenInit2CarriesTheHint(t *testing.T) {
	bs, err := EncodeNegTokenInit2(mechTypes)
	if err != nil {
		t.Fatalf("could not write the token: %v", err)
	}

	if !bytes.Contains(bs, []byte("not_defined_in_RFC4178@please_ignore")) {
		t.Error("the token went out without the hint every other implementation sends")
	}
}

// TestNegTokenRespRoundTrip is the second leg, which carries the challenge out and the state of
// the exchange back.
func TestNegTokenRespRoundTrip(t *testing.T) {
	token := []byte("the NTLM challenge message")
	mic := []byte("the mechanism list check")

	bs, err := EncodeNegTokenResp(0x01, NlmpOid, token, mic)
	if err != nil {
		t.Fatalf("could not write the token: %v", err)
	}

	resp, err := DecodeNegTokenResp(bs)
	if err != nil {
		t.Fatalf("could not read back what was written: %v", err)
	}

	if resp.NegState != 0x01 {
		t.Errorf("the state came back %d, want 1", resp.NegState)
	}
	if !resp.SupportedMech.Equal(NlmpOid) {
		t.Errorf("the mechanism came back %v, want NTLM", resp.SupportedMech)
	}
	if !bytes.Equal(resp.ResponseToken, token) {
		t.Errorf("the inner token came back %q, want %q", resp.ResponseToken, token)
	}
	if !bytes.Equal(resp.MechListMIC, mic) {
		t.Errorf("the check came back %q, want %q", resp.MechListMIC, mic)
	}
}

// TestEncodeNegTokenRespStripsTheOuterWrapper is what the trimming at the end of it is for. The
// response is written as a whole context token and then the application wrapper is cut off, so
// what goes out starts at the tagged choice — which is exactly what the reader is told to expect,
// and what a client is waiting for.
func TestEncodeNegTokenRespStripsTheOuterWrapper(t *testing.T) {
	bs, err := EncodeNegTokenResp(0x01, NlmpOid, []byte("a challenge"), nil)
	if err != nil {
		t.Fatalf("could not write the token: %v", err)
	}

	// 0xa1 is [1], the negTokenResp arm of the choice.
	if bs[0] != 0xa1 {
		t.Errorf("the token opens with %#02x, want 0xa1", bs[0])
	}
}

// TestFinalNegTokenRespIsAnAcceptance is the constant sent to close a session setup. It is
// written out by hand rather than encoded, so what it decodes to is worth checking: a client
// reads it to learn the exchange is over and it succeeded.
func TestFinalNegTokenRespIsAnAcceptance(t *testing.T) {
	resp, err := DecodeNegTokenResp(FinalNegTokenResp)
	if err != nil {
		t.Fatalf("the token sent to end a session setup does not decode: %v", err)
	}

	if resp.NegState != 0 {
		t.Errorf("the state is %d, want 0 for acceptance", resp.NegState)
	}
}

// TestDecodeNegTokenInitRefusesATokenWithNothingInIt is a token that is well formed and carries
// no inner token. The field is marked optional, so it decodes without complaint and leaves an
// empty list behind; reaching for the first entry of it regardless is a panic, and there is no
// recover anywhere in the read path, so the panic is not the connection going away but the
// process. Ten bytes off the wire, before anybody has said who they are, took the server down.
func TestDecodeNegTokenInitRefusesATokenWithNothingInIt(t *testing.T) {
	// [APPLICATION 0] SEQUENCE { OID spnego } and nothing else.
	bs := []byte{0x60, 0x08, 0x06, 0x06, 0x2b, 0x06, 0x01, 0x05, 0x05, 0x02}

	init, err := DecodeNegTokenInit(bs)
	if err == nil {
		t.Fatalf("a token carrying nothing was read as %v", init)
	}
	if init != nil {
		t.Error("a token that was refused came back with something all the same")
	}
}

// TestDecodeNegTokenInit2RefusesATokenWithNothingInIt is the same token read the other way.
func TestDecodeNegTokenInit2RefusesATokenWithNothingInIt(t *testing.T) {
	bs := []byte{0x60, 0x08, 0x06, 0x06, 0x2b, 0x06, 0x01, 0x05, 0x05, 0x02}

	init, err := DecodeNegTokenInit2(bs)
	if err == nil {
		t.Fatalf("a token carrying nothing was read as %v", init)
	}
	if init != nil {
		t.Error("a token that was refused came back with something all the same")
	}
}

// TestDecodingRefusesRubbish walks the shapes a token arrives in when it is not one. Every one of
// these is read before the client has authenticated, so the whole of what is asked here is that
// an answer comes back at all rather than the process ending.
func TestDecodingRefusesRubbish(t *testing.T) {
	for _, tt := range []struct {
		name string
		bs   []byte
	}{
		{"nothing at all", nil},
		{"a single byte", []byte{0x60}},
		{"a length and nothing behind it", []byte{0x60, 0x40}},
		{"a length longer than what follows", []byte{0x60, 0x7f, 0x06, 0x01}},
		{"the wrong outer tag", []byte{0x30, 0x08, 0x06, 0x06, 0x2b, 0x06, 0x01, 0x05, 0x05, 0x02}},
		{"a truncated object identifier", []byte{0x60, 0x08, 0x06, 0x06, 0x2b, 0x06}},
		{"a length that says it is enormous", []byte{0x60, 0x84, 0xff, 0xff, 0xff, 0xff, 0x06}},
		{"nothing but zeroes", make([]byte, 32)},
		{"nothing but ones", bytes.Repeat([]byte{0xff}, 32)},
		{"text", []byte("this is not a token at all")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Each of the four readers is handed the same bytes. None of them may end the
			// process, and the result is whatever it is as long as there is one.
			if _, err := DecodeNegTokenInit(tt.bs); err == nil {
				t.Log("read as a negTokenInit without complaint")
			}
			if _, err := DecodeNegTokenInit2(tt.bs); err == nil {
				t.Log("read as a negTokenInit2 without complaint")
			}
			if _, err := DecodeNegTokenResp(tt.bs); err == nil {
				t.Log("read as a negTokenResp without complaint")
			}
		})
	}
}

// FuzzDecodeNegToken walks the bytes of a security buffer, which is handed to these readers
// straight out of a session setup request before the client has authenticated. The property is
// that a reader answers rather than ending the process: a panic here is not one connection going
// away, since nothing in the read path recovers.
func FuzzDecodeNegToken(f *testing.F) {
	init, err := EncodeNegTokenInit(mechTypes, []byte("the NTLM negotiate message"))
	if err != nil {
		f.Fatalf("could not write a seed: %v", err)
	}
	f.Add(init)

	init2, err := EncodeNegTokenInit2(mechTypes)
	if err != nil {
		f.Fatalf("could not write a seed: %v", err)
	}
	f.Add(init2)

	resp, err := EncodeNegTokenResp(0x01, NlmpOid, []byte("the challenge"), nil)
	if err != nil {
		f.Fatalf("could not write a seed: %v", err)
	}
	f.Add(resp)

	f.Add(FinalNegTokenResp)
	f.Add([]byte{0x60, 0x08, 0x06, 0x06, 0x2b, 0x06, 0x01, 0x05, 0x05, 0x02})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, bs []byte) {
		// A reader that hands something back has to hand back something usable: the caller at
		// conn.go reaches straight into what it is given without looking first.
		if got, err := DecodeNegTokenInit(bs); err == nil && got == nil {
			t.Fatal("a negTokenInit was read without complaint and came back as nothing")
		}
		if got, err := DecodeNegTokenInit2(bs); err == nil && got == nil {
			t.Fatal("a negTokenInit2 was read without complaint and came back as nothing")
		}
		if got, err := DecodeNegTokenResp(bs); err == nil && got == nil {
			t.Fatal("a negTokenResp was read without complaint and came back as nothing")
		}
	})
}
