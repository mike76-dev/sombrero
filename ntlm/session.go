// Taken from https://github.com/hirochachacha/go-smb2
package ntlm

import (
	"bytes"
	"crypto/rc4"
	"encoding/binary"
	"errors"

	"github.com/oiweiwei/go-msrpc/msrpc/dtyp"
	"golang.org/x/crypto/blake2b"
)

// Session represents an NTLM authentication session.
type Session struct {
	isClientSide bool

	user   string
	domain string

	negotiateFlags     uint32
	exportedSessionKey []byte
	clientSigningKey   []byte
	serverSigningKey   []byte

	clientHandle *rc4.Cipher
	serverHandle *rc4.Cipher
}

// SecurityContext represents the context of the user authenticated by the server.
type SecurityContext struct {
	User       string
	Domain     string
	UserRID    uint32
	DomainSID  *dtyp.SID
	SessionKey []byte
}

// GetSecurityContext generates a security context from the session data.
func (s *Session) GetSecurityContext() (sc SecurityContext) {
	if s.user == "" {
		return
	}

	h, _ := blake2b.New256(nil)
	h.Write([]byte(s.user))
	if s.domain == "" {
		hash := h.Sum(nil)
		return SecurityContext{
			User:    s.user,
			UserRID: binary.LittleEndian.Uint32(hash[:4]),
			DomainSID: &dtyp.SID{
				Revision:          1,
				SubAuthorityCount: 2,
				IDAuthority: &dtyp.SIDIDAuthority{
					Value: []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x05},
				},
				SubAuthority: []uint32{0, 0},
			},
			SessionKey: s.exportedSessionKey,
		}
	}

	h.Write([]byte(s.domain))
	hash := h.Sum(nil)
	return SecurityContext{
		User:    s.user,
		Domain:  s.domain,
		UserRID: binary.LittleEndian.Uint32(hash[:4]),
		DomainSID: &dtyp.SID{
			Revision:          1,
			SubAuthorityCount: 4,
			IDAuthority: &dtyp.SIDIDAuthority{
				Value: []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x05},
			},
			SubAuthority: []uint32{
				0x15,
				binary.LittleEndian.Uint32(hash[4:8]),
				binary.LittleEndian.Uint32(hash[8:12]),
				binary.LittleEndian.Uint32(hash[12:16]),
			},
		},
		SessionKey: s.exportedSessionKey,
	}
}

// User returns the session's username.
func (s *Session) User() string {
	return s.user
}

// Domain returns the session's domain name.
func (s *Session) Domain() string {
	return s.domain
}

// SessionKey returns the session's signing key.
func (s *Session) SessionKey() []byte {
	return s.exportedSessionKey
}

// Sum generates a checksum of the provided message.
func (s *Session) Sum(plaintext []byte, seqNum uint32) ([]byte, uint32) {
	if s.negotiateFlags&NTLMSSP_NEGOTIATE_SIGN == 0 {
		return nil, 0
	}

	if s.isClientSide {
		return mac(nil, s.negotiateFlags, s.clientHandle, s.clientSigningKey, seqNum, plaintext)
	}
	return mac(nil, s.negotiateFlags, s.serverHandle, s.serverSigningKey, seqNum, plaintext)
}

// CheckSum verifies the checksum provided by the client.
func (s *Session) CheckSum(sum, plaintext []byte, seqNum uint32) (bool, uint32) {
	if s.negotiateFlags&NTLMSSP_NEGOTIATE_SIGN == 0 {
		if sum == nil {
			return true, 0
		}
		return false, 0
	}

	if s.isClientSide {
		ret, seqNum := mac(nil, s.negotiateFlags, s.serverHandle, s.serverSigningKey, seqNum, plaintext)
		if !bytes.Equal(sum, ret) {
			return false, 0
		}
		return true, seqNum
	}
	ret, seqNum := mac(nil, s.negotiateFlags, s.clientHandle, s.clientSigningKey, seqNum, plaintext)
	if !bytes.Equal(sum, ret) {
		return false, 0
	}
	return true, seqNum
}

// Seal encrypts a message.
func (s *Session) Seal(dst, plaintext []byte, seqNum uint32) ([]byte, uint32) {
	ret, ciphertext := sliceForAppend(dst, len(plaintext)+16)

	switch {
	case s.negotiateFlags&NTLMSSP_NEGOTIATE_SEAL != 0:
		s.clientHandle.XORKeyStream(ciphertext[16:], plaintext)

		if s.isClientSide {
			_, seqNum = mac(ciphertext[:0], s.negotiateFlags, s.clientHandle, s.clientSigningKey, seqNum, plaintext)
		} else {
			_, seqNum = mac(ciphertext[:0], s.negotiateFlags, s.serverHandle, s.serverSigningKey, seqNum, plaintext)
		}
	case s.negotiateFlags&NTLMSSP_NEGOTIATE_SIGN != 0:
		copy(ciphertext[16:], plaintext)

		if s.isClientSide {
			_, seqNum = mac(ciphertext[:0], s.negotiateFlags, s.clientHandle, s.clientSigningKey, seqNum, plaintext)
		} else {
			_, seqNum = mac(ciphertext[:0], s.negotiateFlags, s.serverHandle, s.serverSigningKey, seqNum, plaintext)
		}
	}

	return ret, seqNum
}

// Unseal decrypts a message.
func (s *Session) Unseal(dst, ciphertext []byte, seqNum uint32) ([]byte, uint32, error) {
	ret, plaintext := sliceForAppend(dst, len(ciphertext)-16)

	switch {
	case s.negotiateFlags&NTLMSSP_NEGOTIATE_SEAL != 0:
		s.serverHandle.XORKeyStream(plaintext, ciphertext[16:])

		var sum []byte

		if s.isClientSide {
			sum, seqNum = mac(nil, s.negotiateFlags, s.serverHandle, s.serverSigningKey, seqNum, plaintext)
		} else {
			sum, seqNum = mac(nil, s.negotiateFlags, s.clientHandle, s.clientSigningKey, seqNum, plaintext)
		}
		if !bytes.Equal(ciphertext[:16], sum) {
			return nil, 0, errors.New("signature mismatch")
		}
	case s.negotiateFlags&NTLMSSP_NEGOTIATE_SIGN != 0:
		copy(plaintext, ciphertext[16:])

		var sum []byte

		if s.isClientSide {
			sum, seqNum = mac(nil, s.negotiateFlags, s.serverHandle, s.serverSigningKey, seqNum, plaintext)
		} else {
			sum, seqNum = mac(nil, s.negotiateFlags, s.clientHandle, s.clientSigningKey, seqNum, plaintext)
		}
		if !bytes.Equal(ciphertext[:16], sum) {
			return nil, 0, errors.New("signature mismatch")
		}
	default:
		copy(plaintext, ciphertext[16:])
		for _, s := range ciphertext[:16] {
			if s != 0x0 {
				return nil, 0, errors.New("signature mismatch")
			}
		}
	}

	return ret, seqNum, nil
}
