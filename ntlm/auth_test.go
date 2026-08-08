package ntlm

import (
	"crypto/hmac"
	"crypto/md5"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/utils"
	"golang.org/x/crypto/md4"
)

const (
	testUser           = "alice"
	testPassword       = "hunter2"
	testWorkgroup      = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	testOtherWorkgroup = "9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d"
	testWorkgroupName  = "wrg"
)

// stubStore answers every account lookup with the account and error it was built with, whatever
// is asked of it, and resolves the one workgroup name it was given.
type stubStore struct {
	acc stores.Account
	err error

	// name is the name of the workgroup testWorkgroup, if it is a named one. It is held in the
	// form the stores keep it in, so that the lookup below folds its argument the way the real
	// ones do.
	name string

	// lookedUp records the workgroup the account lookup was made under, so that a test can tell
	// that the name the client sent was turned into the UUID before the store saw it.
	lookedUp *string
}

func (s stubStore) FindAccount(_, workgroup string) (stores.Account, error) {
	if s.lookedUp != nil {
		*s.lookedUp = workgroup
	}
	return s.acc, s.err
}

func (s stubStore) FindWorkgroupByName(name string) (stores.Workgroup, error) {
	if s.name == "" || stores.NormalizeWorkgroupName(name) != s.name {
		return stores.Workgroup{}, nil // as the stores report a workgroup that is not there
	}
	return stores.Workgroup{ID: 1, UUID: uuid.MustParse(testWorkgroup), Name: s.name}, nil
}

// knownAccount is the store as it behaves when the account is there.
func knownAccount() stubStore {
	h := md4.New()
	h.Write(utils.EncodeStringToBytes(testPassword))
	return stubStore{
		acc: stores.Account{
			ID:        1,
			Username:  testUser,
			NTHash:    h.Sum(nil),
			Workgroup: testWorkgroup,
		},
		name: testWorkgroupName,
	}
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

// A client logs in as <workgroup>\<user>, and the workgroup part may be the name of a named
// workgroup rather than its UUID. The name has to be resolved here, because everything below the
// session - the account lookup, the security maps, the per-workgroup share connections - is keyed
// by the UUID and by nothing else.
//
// The case the client sends is not the server's business either: a domain name is not
// case-sensitive, and Windows sends the workgroup uppercased whatever the user typed.
//
// The response the client signs is computed over the domain exactly as it put it on the wire,
// which is what authenticateAs does. So this also pins down that resolving the name does not
// reach the key: a server that fed the UUID into NTOWFv2 instead would turn this login away.
func TestAuthenticateResolvesWorkgroupName(t *testing.T) {
	for _, domain := range []string{"wrg", "WRG", "Wrg"} {
		var lookedUp string
		store := knownAccount()
		store.lookedUp = &lookedUp
		srv := NewServer("SOMBRERO", "WORKGROUP", store)

		if err := authenticateAs(t, srv, testUser, domain, ntHashOf(testPassword)); err != nil {
			t.Fatalf("login as %q\\%q was turned away: %v", domain, testUser, err)
		}
		if lookedUp != testWorkgroup {
			t.Errorf("account for %q looked up under workgroup %q, want %q", domain, lookedUp, testWorkgroup)
		}
		if got := srv.Session().Domain(); got != testWorkgroup {
			t.Errorf("session workgroup for %q: got %q, want %q", domain, got, testWorkgroup)
		}
	}
}

// A workgroup UUID reaches the session in its canonical form, whatever spelling the client chose.
// uuid.Parse takes the hyphenless and the braced forms too, and the keys the share connections are
// held under are canonical, so passing the client's own spelling on would leave a session that
// cannot find the connection its workgroup has.
func TestAuthenticateCanonicalizesWorkgroupUUID(t *testing.T) {
	for _, domain := range []string{
		testWorkgroup,
		"3F2504E04F8911D39A0C0305E82C3301",
		"{3f2504e0-4f89-11d3-9a0c-0305e82c3301}",
	} {
		var lookedUp string
		store := knownAccount()
		store.lookedUp = &lookedUp
		srv := NewServer("SOMBRERO", "WORKGROUP", store)

		if err := authenticateAs(t, srv, testUser, domain, ntHashOf(testPassword)); err != nil {
			t.Fatalf("login with workgroup %q was turned away: %v", domain, err)
		}
		if lookedUp != testWorkgroup {
			t.Errorf("account for %q looked up under workgroup %q, want %q", domain, lookedUp, testWorkgroup)
		}
		if got := srv.Session().Domain(); got != testWorkgroup {
			t.Errorf("session workgroup for %q: got %q, want %q", domain, got, testWorkgroup)
		}
	}
}

// A workgroup name that resolves to nothing must not become an account lookup. The stores report a
// workgroup that is not there as a zero Workgroup, whose UUID is the zero UUID: a well-formed
// workgroup key that an account could be found under, and one the client picks by sending a name
// that does not exist.
func TestAuthenticateRejectsAnUnknownWorkgroupName(t *testing.T) {
	var lookedUp string
	store := knownAccount()
	store.lookedUp = &lookedUp
	srv := NewServer("SOMBRERO", "WORKGROUP", store)

	if err := authenticateAs(t, srv, testUser, "no-such-workgroup", ntHashOf(testPassword)); err == nil {
		t.Error("a workgroup that does not exist authenticated")
	}
	if lookedUp != "" {
		t.Errorf("the account was looked up under workgroup %q", lookedUp)
	}
}

// The same login against a real store rather than a stub, which is what catches the two halves of
// the name being kept and looked up in different forms: the store folds the name it is given, and
// the server hands it the domain off the wire.
func TestAuthenticateAgainstTheStore(t *testing.T) {
	store, err := stores.NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatalf("could not create the store: %v", err)
	}
	t.Cleanup(store.Close)

	u := uuid.New()
	if err := store.AddWorkgroup(stores.Workgroup{UUID: u, Name: testWorkgroupName}); err != nil {
		t.Fatalf("could not add the workgroup: %v", err)
	}
	if err := store.AddAccount(stores.Account{
		Username:  testUser,
		Password:  testPassword,
		Workgroup: u.String(),
	}); err != nil {
		t.Fatalf("could not add the account: %v", err)
	}

	// The name as a client sends it, the name as it was created, and the UUID. Windows uppercases
	// the workgroup, so the first of these is the one the bug was found with.
	for _, domain := range []string{strings.ToUpper(testWorkgroupName), testWorkgroupName, u.String()} {
		srv := NewServer("SOMBRERO", "WORKGROUP", store)

		if err := authenticateAs(t, srv, testUser, domain, ntHashOf(testPassword)); err != nil {
			t.Errorf("login as %q\\%q was turned away: %v", domain, testUser, err)
			continue
		}
		if got := srv.Session().Domain(); got != u.String() {
			t.Errorf("session workgroup for %q: got %q, want %q", domain, got, u)
		}
	}

	// A password that is not the account's still fails, whichever way the workgroup was named.
	srv := NewServer("SOMBRERO", "WORKGROUP", store)
	if err := authenticateAs(t, srv, testUser, testWorkgroupName, ntHashOf("not the password")); err == nil {
		t.Error("a wrong password authenticated against a named workgroup")
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
