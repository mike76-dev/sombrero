// Taken from https://github.com/hirochachacha/go-smb2
package ntlm

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"

	"crypto/rc4"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"strings"
	"time"

	"github.com/mike76-dev/sombrero/spnego"
	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/utils"
	"lukechampine.com/frand"
)

// Server is an NTLMv2 authentication server.
type Server struct {
	targetName   string
	targetDomain string

	nmsg    []byte
	cmsg    []byte
	amsg    []byte
	session *Session

	mechTypes []asn1.ObjectIdentifier
	accounts  AccountStore
}

// AccountStore defines the minimal account store.
type AccountStore interface {
	FindAccount(username, workgroup string) (stores.Account, error)
}

// NewServer returns an initialized NTLMv2 server.
func NewServer(targetName, targetDomain string, store AccountStore) *Server {
	mechTypes := make([]asn1.ObjectIdentifier, 1)
	mechTypes[0] = spnego.NlmpOid
	return &Server{
		targetName:   targetName,
		targetDomain: targetDomain,
		mechTypes:    mechTypes,
		accounts:     store,
	}
}

// Negotiate generates an NTLM NEGOTIATE message to be put in SMB2_NEGOTIATE response.
func (s *Server) Negotiate() ([]byte, error) {
	return spnego.EncodeNegTokenInit2(s.mechTypes)
}

// Challenge generates an NTLM CHALLENGE message to be put in SMB2_SESSION_SETUP response Part 1.
func (s *Server) Challenge(nmsg []byte) (cmsg []byte, err error) {
	//        NegotiateMessage
	//   0-8: Signature
	//  8-12: MessageType
	// 12-16: NegotiateFlags
	// 16-24: DomainNameFields
	// 24-32: WorkstationFields
	// 32-40: Version
	//   40-: Payload

	s.nmsg = nmsg

	if len(nmsg) < 32 {
		return nil, errors.New("message length is too short")
	}

	if !bytes.Equal(nmsg[:8], signature) {
		return nil, errors.New("invalid signature")
	}

	if binary.LittleEndian.Uint32(nmsg[8:12]) != NtLmNegotiate {
		return nil, errors.New("invalid message type")
	}

	flags := binary.LittleEndian.Uint32(nmsg[12:16]) & defaultFlags
	flags |= NTLMSSP_NEGOTIATE_TARGET_INFO
	flags |= NTLMSSP_TARGET_TYPE_SERVER

	//        ChallengeMessage
	//   0-8: Signature
	//  8-12: MessageType
	// 12-20: TargetNameFields
	// 20-24: NegotiateFlags
	// 24-32: ServerChallenge
	// 32-40: _
	// 40-48: TargetInfoFields
	// 48-56: Version
	//   56-: Payload

	off := 48

	if flags&NTLMSSP_NEGOTIATE_VERSION != 0 {
		off += 8
	}

	targetName := utils.EncodeStringToBytes(s.targetName)
	targetDomain := utils.EncodeStringToBytes(s.targetDomain)
	targetNameLow := utils.EncodeStringToBytes(strings.ToLower(s.targetName))

	length := len(targetName)
	if flags&NTLMSSP_NEGOTIATE_TARGET_INFO != 0 {
		length += len(targetName) + 4    // MsvAvNbComputerName
		length += len(targetName) + 4    // MsvAvNbDomainName
		length += len(targetNameLow) + 4 // MsvAvDnsComputerName
		length += len(targetDomain) + 4  // MsvAvDnsDomainName
		length += 8 + 4                  // MsvAvTimestamp
		length += 4                      // MsvAvEOL
	}

	cmsg = make([]byte, off+length)

	copy(cmsg[:8], signature)
	binary.LittleEndian.PutUint32(cmsg[8:12], NtLmChallenge)
	binary.LittleEndian.PutUint32(cmsg[20:24], flags)

	if targetName != nil && flags&NTLMSSP_REQUEST_TARGET != 0 {
		length := copy(cmsg[off:off+len(targetName)], targetName)
		binary.LittleEndian.PutUint16(cmsg[12:14], uint16(length))
		binary.LittleEndian.PutUint16(cmsg[14:16], uint16(length))
		binary.LittleEndian.PutUint32(cmsg[16:20], uint32(off))
		off += length
	}

	if flags&NTLMSSP_NEGOTIATE_TARGET_INFO != 0 {
		offset := off
		binary.LittleEndian.PutUint16(cmsg[offset:offset+2], MsvAvNbComputerName)
		binary.LittleEndian.PutUint16(cmsg[offset+2:offset+4], uint16(len(targetName)))
		copy(cmsg[offset+4:offset+4+len(targetName)], targetName)
		length := 4 + len(targetName)
		offset += length

		binary.LittleEndian.PutUint16(cmsg[offset:offset+2], MsvAvNbDomainName)
		binary.LittleEndian.PutUint16(cmsg[offset+2:offset+4], uint16(len(targetName)))
		copy(cmsg[offset+4:offset+4+len(targetName)], targetName)
		length += 4 + len(targetName)
		offset += 4 + len(targetName)

		binary.LittleEndian.PutUint16(cmsg[offset:offset+2], MsvAvDnsComputerName)
		binary.LittleEndian.PutUint16(cmsg[offset+2:offset+4], uint16(len(targetNameLow)))
		copy(cmsg[offset+4:offset+4+len(targetNameLow)], targetNameLow)
		length += 4 + len(targetNameLow)
		offset += 4 + len(targetNameLow)

		binary.LittleEndian.PutUint16(cmsg[offset:offset+2], MsvAvDnsDomainName)
		binary.LittleEndian.PutUint16(cmsg[offset+2:offset+4], uint16(len(targetDomain)))
		copy(cmsg[offset+4:offset+4+len(targetDomain)], targetDomain)
		length += 4 + len(targetDomain)
		offset += 4 + len(targetDomain)

		binary.LittleEndian.PutUint16(cmsg[offset:offset+2], MsvAvTimestamp)
		binary.LittleEndian.PutUint16(cmsg[offset+2:offset+4], 8)
		binary.LittleEndian.PutUint64(cmsg[offset+4:offset+12], utils.UnixToFiletime(time.Now()))
		length += 12
		offset += 12

		copy(cmsg[offset:offset+4], []byte{0x00, 0x00, 0x00, 0x00}) // AvId: MsvAvEOL, AvLen: 0
		length += 4
		binary.LittleEndian.PutUint16(cmsg[40:42], uint16(length))
		binary.LittleEndian.PutUint16(cmsg[42:44], uint16(length))
		binary.LittleEndian.PutUint32(cmsg[44:48], uint32(off))
	}

	frand.Read(cmsg[24:32])

	if flags&NTLMSSP_NEGOTIATE_VERSION != 0 {
		copy(cmsg[48:56], version)
	}

	s.cmsg = cmsg

	return cmsg, nil
}

// Authenticate processes the client's challenge received in the SMB2_SESSION_SETUP request Part 2.
func (s *Server) Authenticate(amsg []byte) (err error) {
	//        AuthenticateMessage
	//   0-8: Signature
	//  8-12: MessageType
	// 12-20: LmChallengeResponseFields
	// 20-28: NtChallengeResponseFields
	// 28-36: DomainNameFields
	// 36-44: UserNameFields
	// 44-52: WorkstationFields
	// 52-60: EncryptedRandomSessionKeyFields
	// 60-64: NegotiateFlags
	// 64-72: Version
	// 72-88: MIC
	//   88-: Payload

	if len(amsg) < 64 {
		return errors.New("message is too short")
	}

	if !bytes.Equal(amsg[:8], signature) {
		return errors.New("invalid signature")
	}

	if binary.LittleEndian.Uint32(amsg[8:12]) != NtLmAuthenticate {
		return errors.New("invalid message type")
	}

	flags := binary.LittleEndian.Uint32(amsg[60:64])

	ntChallengeResponseLen := binary.LittleEndian.Uint16(amsg[20:22])    // amsg.NtChallengeResponseLen
	ntChallengeResponseMaxLen := binary.LittleEndian.Uint16(amsg[22:24]) // amsg.NtChallengeResponseMaxLen
	if ntChallengeResponseMaxLen < ntChallengeResponseLen {
		return errors.New("invalid NT challenge format")
	}
	ntChallengeResponseBufferOffset := binary.LittleEndian.Uint32(amsg[24:28]) // amsg.NtChallengeResponseBufferOffset
	if len(amsg) < int(ntChallengeResponseBufferOffset+uint32(ntChallengeResponseLen)) {
		return errors.New("invalid NT challenge format")
	}
	ntChallengeResponse := amsg[ntChallengeResponseBufferOffset : ntChallengeResponseBufferOffset+uint32(ntChallengeResponseLen)] // amsg.NtChallengeResponse

	domainNameLen := binary.LittleEndian.Uint16(amsg[28:30])    // amsg.DomainNameLen
	domainNameMaxLen := binary.LittleEndian.Uint16(amsg[30:32]) // amsg.DomainNameMaxLen
	if domainNameMaxLen < domainNameLen {
		return errors.New("invalid domain name format")
	}
	domainNameBufferOffset := binary.LittleEndian.Uint32(amsg[32:36]) // amsg.DomainNameBufferOffset
	if len(amsg) < int(domainNameBufferOffset+uint32(domainNameLen)) {
		return errors.New("invalid domain name format")
	}
	domainName := amsg[domainNameBufferOffset : domainNameBufferOffset+uint32(domainNameLen)] // amsg.DomainName

	userNameLen := binary.LittleEndian.Uint16(amsg[36:38])    // amsg.UserNameLen
	userNameMaxLen := binary.LittleEndian.Uint16(amsg[38:40]) // amsg.UserNameMaxLen
	if userNameMaxLen < userNameLen {
		return errors.New("invalid user name format")
	}
	userNameBufferOffset := binary.LittleEndian.Uint32(amsg[40:44]) // amsg.UserNameBufferOffset
	if len(amsg) < int(userNameBufferOffset+uint32(userNameLen)) {
		return errors.New("invalid user name format")
	}
	userName := amsg[userNameBufferOffset : userNameBufferOffset+uint32(userNameLen)] // amsg.UserName

	encryptedRandomSessionKeyLen := binary.LittleEndian.Uint16(amsg[52:54])    // amsg.EncryptedRandomSessionKeyLen
	encryptedRandomSessionKeyMaxLen := binary.LittleEndian.Uint16(amsg[54:56]) // amsg.EncryptedRandomSessionKeyMaxLen
	if encryptedRandomSessionKeyMaxLen < encryptedRandomSessionKeyLen {
		return errors.New("invalid session key format")
	}
	encryptedRandomSessionKeyBufferOffset := binary.LittleEndian.Uint32(amsg[56:60]) // amsg.EncryptedRandomSessionKeyBufferOffset
	if len(amsg) < int(encryptedRandomSessionKeyBufferOffset+uint32(encryptedRandomSessionKeyLen)) {
		return errors.New("invalid session key format")
	}
	encryptedRandomSessionKey := amsg[encryptedRandomSessionKeyBufferOffset : encryptedRandomSessionKeyBufferOffset+uint32(encryptedRandomSessionKeyLen)] // amsg.EncryptedRandomSessionKey

	if len(userName) != 0 || len(ntChallengeResponse) != 0 {
		user := strings.ToLower(utils.DecodeToString(userName))
		var domain string
		if len(domainName) != 0 {
			domain = strings.ToLower(utils.DecodeToString(domainName))
		}

		acc, err := s.accounts.FindAccount(user, domain)
		if err != nil {
			return err
		}

		// A lookup that came back with nothing must never reach the comparison below. The key
		// it would be built from is NTOWFv2 over a nil hash, and the user name and domain that
		// go into it are both the client's own: the whole of it is computable by whoever is
		// asking, so every unknown user would authenticate. The store reports this as
		// ErrAccountNotFound and is caught above; this is here so that a store that ever goes
		// back to reporting it as a zero Account fails closed.
		if len(acc.NTHash) == 0 {
			return errors.New("login failure")
		}

		expectedNtChallengeResponse := make([]byte, len(ntChallengeResponse))
		ntlmv2ClientChallenge := ntChallengeResponse[16:]
		USER := utils.EncodeStringToBytes(strings.ToUpper(user))
		h := hmac.New(md5.New, ntowfv2Hash(USER, acc.NTHash, domainName))
		serverChallenge := s.cmsg[24:32]
		timeStamp := ntlmv2ClientChallenge[8:16]
		clientChallenge := ntlmv2ClientChallenge[16:24]
		targetInfo := ntlmv2ClientChallenge[28:]
		encodeNtlmv2Response(expectedNtChallengeResponse, h, serverChallenge, clientChallenge, timeStamp, bytesEncoder(targetInfo))
		if !bytes.Equal(ntChallengeResponse, expectedNtChallengeResponse) {
			return errors.New("login failure")
		}

		session := new(Session)

		session.isClientSide = false

		session.user = user
		session.domain = domain
		session.negotiateFlags = flags

		h.Reset()
		h.Write(ntChallengeResponse[:16])
		sessionBaseKey := h.Sum(nil)

		keyExchangeKey := sessionBaseKey // if ntlm version == 2

		if flags&NTLMSSP_NEGOTIATE_KEY_EXCH != 0 {
			session.exportedSessionKey = make([]byte, 16)
			cipher, err := rc4.NewCipher(keyExchangeKey)
			if err != nil {
				return err
			}
			cipher.XORKeyStream(session.exportedSessionKey, encryptedRandomSessionKey)
		} else {
			session.exportedSessionKey = keyExchangeKey
		}

		if infoMap, ok := parseAvPairs(targetInfo); ok {
			if avFlags, ok := infoMap[MsvAvFlags]; ok && binary.LittleEndian.Uint32(avFlags)&0x02 != 0 {
				MIC := make([]byte, 16)
				if flags&NTLMSSP_NEGOTIATE_VERSION != 0 {
					copy(MIC, amsg[72:88])
					copy(amsg[72:88], zero[:])
				} else {
					copy(MIC, amsg[64:80])
					copy(amsg[64:80], zero[:])
				}
				h = hmac.New(md5.New, session.exportedSessionKey)
				h.Write(s.nmsg)
				h.Write(s.cmsg)
				h.Write(amsg)
				if !bytes.Equal(MIC, h.Sum(nil)) {
					return errors.New("login failure")
				}
			}
			session.infoMap = infoMap
		}

		{
			session.clientSigningKey = signKey(flags, session.exportedSessionKey, true)
			session.serverSigningKey = signKey(flags, session.exportedSessionKey, false)

			session.clientHandle, err = rc4.NewCipher(sealKey(flags, session.exportedSessionKey, true))
			if err != nil {
				return err
			}

			session.serverHandle, err = rc4.NewCipher(sealKey(flags, session.exportedSessionKey, false))
			if err != nil {
				return err
			}
		}

		s.session = session
		s.amsg = amsg

		return nil
	}

	return errors.New("credential is empty")
}

// Signature generates a signature of an NTLM AUTHENTICATE message.
func (s *Server) Signature() []byte {
	h := hmac.New(md5.New, s.session.SessionKey())
	h.Reset()
	h.Write(s.nmsg)
	h.Write(s.cmsg)

	off := 64
	flags := binary.LittleEndian.Uint32(s.amsg[60:64])
	if flags&NTLMSSP_NEGOTIATE_VERSION != 0 {
		off = 72
	}

	h.Write(s.amsg[:off])
	h.Write(zero[:])
	h.Write(s.amsg[off+16:])

	return h.Sum(nil)
}

// Session returns the NTLM server's authentication session.
func (s *Server) Session() *Session {
	return s.session
}
