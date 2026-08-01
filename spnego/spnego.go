// Taken from github.com/hirochachacha/go-smb2
package spnego

import (
	"encoding/asn1"
	"errors"

	"github.com/geoffgarside/ber"
)

var (
	SpnegoOid = asn1.ObjectIdentifier([]int{1, 3, 6, 1, 5, 5, 2})
	NlmpOid   = asn1.ObjectIdentifier([]int{1, 3, 6, 1, 4, 1, 311, 2, 2, 10})
)

type initialContextToken struct { // `asn1:"application,tag:0"`
	ThisMech asn1.ObjectIdentifier `asn1:"optional"`
	Init     []NegTokenInit        `asn1:"optional,explict,tag:0"`
	Resp     []NegTokenResp        `asn1:"optional,explict,tag:1"`
}

type initialContextToken2 struct { // `asn1:"application,tag:0"`
	ThisMech asn1.ObjectIdentifier `asn1:"optional"`
	Init2    []NegTokenInit2       `asn1:"optional,explict,tag:0"`
	Resp     []NegTokenResp        `asn1:"optional,explict,tag:1"`
}

// initialContextToken ::= [APPLICATION 0] IMPLICIT SEQUENCE {
//   ThisMech          MechType
//   InnerContextToken negotiateToken
// }

// negotiateToken ::= CHOICE {
//   NegTokenInit [0] NegTokenInit
//   NegTokenResp [1] NegTokenResp
// }

type NegTokenInit struct {
	MechTypes   []asn1.ObjectIdentifier `asn1:"explicit,optional,tag:0"`
	ReqFlags    asn1.BitString          `asn1:"explicit,optional,tag:1"`
	MechToken   []byte                  `asn1:"explicit,optional,tag:2"`
	MechListMIC []byte                  `asn1:"explicit,optional,tag:3"`
}

// "not_defined_in_RFC4178@please_ignore"
var negHints = asn1.RawValue{
	FullBytes: []byte{
		0xa3, 0x2a, 0x30, 0x28, 0xa0, 0x26, 0x1b, 0x24, 0x6e, 0x6f, 0x74, 0x5f, 0x64, 0x65, 0x66, 0x69, 0x6e, 0x65, 0x64, 0x5f,
		0x69, 0x6e, 0x5f, 0x52, 0x46, 0x43, 0x34, 0x31, 0x37, 0x38, 0x40, 0x70, 0x6c, 0x65, 0x61, 0x73,
		0x65, 0x5f, 0x69, 0x67, 0x6e, 0x6f, 0x72, 0x65,
	},
}

// type NegHint struct {
// HintName    string `asn1:"optional,explicit,tag:0"` // GeneralString = 27
// HintAddress []byte `asn1:"optional,explicit,tag:1"`
// }

type NegTokenInit2 struct {
	MechTypes   []asn1.ObjectIdentifier `asn1:"explicit,optional,tag:0"`
	ReqFlags    asn1.BitString          `asn1:"explicit,optional,tag:1"`
	MechToken   []byte                  `asn1:"explicit,optional,tag:2"`
	NegHints    asn1.RawValue           `asn1:"explicit,optional,tag:3"`
	MechListMIC []byte                  `asn1:"explicit,optional,tag:4"`
}

type NegTokenResp struct {
	NegState      asn1.Enumerated       `asn1:"optional,explicit,tag:0"`
	SupportedMech asn1.ObjectIdentifier `asn1:"optional,explicit,tag:1"`
	ResponseToken []byte                `asn1:"optional,explicit,tag:2"`
	MechListMIC   []byte                `asn1:"optional,explicit,tag:3"`
}

func DecodeNegTokenInit2(bs []byte) (*NegTokenInit2, error) {
	var init initialContextToken2

	_, err := ber.UnmarshalWithParams(bs, &init, "application,tag:0")
	if err != nil {
		return nil, err
	}

	// The inner token is marked optional, so a token that carries none of it decodes without
	// complaint and leaves nothing behind. Reaching for the first one regardless is how a peer
	// sending ten well-formed bytes takes the whole server down with it: nothing in the read
	// path recovers, so the panic is not the connection ending but the process.
	if len(init.Init2) == 0 {
		return nil, errors.New("spnego: the token carries no negTokenInit2")
	}

	return &init.Init2[0], nil
}

func EncodeNegTokenInit2(types []asn1.ObjectIdentifier) ([]byte, error) {
	bs, err := asn1.Marshal(
		initialContextToken2{
			ThisMech: SpnegoOid,
			Init2: []NegTokenInit2{
				{
					MechTypes: types,
					NegHints:  negHints,
				},
			},
		})
	if err != nil {
		return nil, err
	}

	bs[0] = 0x60 // `asn1:"application,tag:0"`

	return bs, nil
}

func EncodeNegTokenInit(types []asn1.ObjectIdentifier, token []byte) ([]byte, error) {
	bs, err := asn1.Marshal(
		initialContextToken{
			ThisMech: SpnegoOid,
			Init: []NegTokenInit{
				{
					MechTypes: types,
					MechToken: token,
				},
			},
		})
	if err != nil {
		return nil, err
	}

	bs[0] = 0x60 // `asn1:"application,tag:0"`

	return bs, nil
}

func DecodeNegTokenInit(bs []byte) (*NegTokenInit, error) {
	var init initialContextToken

	_, err := ber.UnmarshalWithParams(bs, &init, "application,tag:0")
	if err != nil {
		return nil, err
	}

	// As above: the inner token is optional and may simply not be there.
	if len(init.Init) == 0 {
		return nil, errors.New("spnego: the token carries no negTokenInit")
	}

	return &init.Init[0], nil
}

func EncodeNegTokenResp(state asn1.Enumerated, typ asn1.ObjectIdentifier, token, mechListMIC []byte) ([]byte, error) {
	bs, err := asn1.Marshal(
		initialContextToken{
			Resp: []NegTokenResp{
				{
					NegState:      state,
					SupportedMech: typ,
					ResponseToken: token,
					MechListMIC:   mechListMIC,
				},
			},
		})
	if err != nil {
		return nil, err
	}

	skip := 1
	if bs[skip] < 128 {
		skip += 1
	} else {
		skip += int(bs[skip]) - 128 + 1
	}

	return bs[skip:], nil
}

func DecodeNegTokenResp(bs []byte) (*NegTokenResp, error) {
	var resp NegTokenResp

	_, err := ber.UnmarshalWithParams(bs, &resp, "explicit,tag:1")
	if err != nil {
		return nil, err
	}

	return &resp, nil
}

// To be returned in a SMB2_SESSION_SETUP response Part 2.
var FinalNegTokenResp = []byte{0xa1, 0x07, 0x30, 0x05, 0xa0, 0x03, 0x0a, 0x01, 0x00}
