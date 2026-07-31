package ntlm

import (
	"crypto/hmac"
	"crypto/md5"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/utils"
	"golang.org/x/crypto/md4"
)

const (
	testUser      = "alice"
	testPassword  = "hunter2"
	testWorkgroup = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
)

// stubStore answers every lookup with the account and error it was built with, whatever is
// asked of it.
type stubStore struct {
	acc stores.Account
	err error
}

func (s stubStore) FindAccount(string, string) (stores.Account, error) { return s.acc, s.err }

// knownAccount is the store as it behaves when the account is there.
func knownAccount() stubStore {
	h := md4.New()
	h.Write(utils.EncodeStringToBytes(testPassword))
	return stubStore{acc: stores.Account{
		ID:        1,
		Username:  testUser,
		NTHash:    h.Sum(nil),
		Workgroup: testWorkgroup,
	}}
}

// authenticateAs drives a whole NTLMv2 exchange against srv on behalf of user, signing the
// challenge with ntHash. Everything it uses is something a client can see or choose, so a hash
// that is not the account's is exactly the position an attacker is in.
func authenticateAs(t *testing.T, srv *Server, user, workgroup string, ntHash []byte) error {
	t.Helper()

	nmsg := make([]byte, 32)
	copy(nmsg[:8], signature)
	binary.LittleEndian.PutUint32(nmsg[8:12], NtLmNegotiate)
	binary.LittleEndian.PutUint32(nmsg[12:16], defaultFlags)

	cmsg, err := srv.Challenge(nmsg)
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}

	serverChallenge := cmsg[24:32]
	tiOff := binary.LittleEndian.Uint32(cmsg[44:48])
	tiLen := binary.LittleEndian.Uint16(cmsg[40:42])
	targetInfo := cmsg[tiOff : tiOff+uint32(tiLen)]

	userBytes := utils.EncodeStringToBytes(user)
	domainBytes := utils.EncodeStringToBytes(workgroup)

	key := ntowfv2Hash(utils.EncodeStringToBytes(strings.ToUpper(user)), ntHash, domainBytes)
	ntResp := make([]byte, 16+28+len(targetInfo))
	h := hmac.New(md5.New, key)
	encodeNtlmv2Response(ntResp, h, serverChallenge, make([]byte, 8), make([]byte, 8), bytesEncoder(targetInfo))

	// No MIC and no session key exchange, so the payload is the three variable-length fields
	// laid end to end after the fixed header.
	const hdr = 64
	amsg := make([]byte, hdr+len(ntResp)+len(domainBytes)+len(userBytes))
	copy(amsg[:8], signature)
	binary.LittleEndian.PutUint32(amsg[8:12], NtLmAuthenticate)
	binary.LittleEndian.PutUint32(amsg[60:64], defaultFlags&^NTLMSSP_NEGOTIATE_VERSION&^NTLMSSP_NEGOTIATE_KEY_EXCH)

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

	return srv.Authenticate(amsg)
}

func ntHashOf(password string) []byte {
	h := md4.New()
	h.Write(utils.EncodeStringToBytes(password))
	return h.Sum(nil)
}

// The positive control. Without it the rejections below would be satisfied by a message this
// test simply builds wrong, and would go on passing however broken the server became.
func TestAuthenticateAcceptsTheRightPassword(t *testing.T) {
	srv := NewServer("SOMBRERO", "WORKGROUP", knownAccount())

	if err := authenticateAs(t, srv, testUser, testWorkgroup, ntHashOf(testPassword)); err != nil {
		t.Fatalf("the account's own password was turned away: %v", err)
	}
	if got := srv.Session().User(); got != testUser {
		t.Errorf("authenticated as %q, want %q", got, testUser)
	}
}

func TestAuthenticateRejectsTheWrongPassword(t *testing.T) {
	srv := NewServer("SOMBRERO", "WORKGROUP", knownAccount())

	if err := authenticateAs(t, srv, testUser, testWorkgroup, ntHashOf("not the password")); err == nil {
		t.Fatal("a wrong password authenticated")
	}
}

// A user that does not exist must not be able to authenticate. The store reports the miss, and
// nothing may treat that as an account.
//
// The hash used here is nil, which is the whole point: it is what an absent account's NTHash
// would be, and it is a value the attacker can supply as readily as the server can. Everything
// else going into the response - the user name, the workgroup, the challenges - is the client's
// own. So this is not a guess at a password; it is the complete credential, computed from
// nothing secret.
func TestAuthenticateRejectsAnUnknownUser(t *testing.T) {
	// Both the shape the stores return now, and the zero-value-and-no-error shape they used
	// to return. The second is what made this an authentication bypass: the lookup missed,
	// and the miss was indistinguishable from a hit.
	for _, store := range []stubStore{
		{err: stores.ErrAccountNotFound},
		{}, // a zero Account and no error
	} {
		srv := NewServer("SOMBRERO", "WORKGROUP", store)

		if err := authenticateAs(t, srv, "nobody", testWorkgroup, nil); err == nil {
			t.Errorf("a user that does not exist authenticated as %q in workgroup %q",
				srv.Session().User(), srv.Session().Domain())
		}
	}
}
