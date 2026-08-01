package ntlm

import (
	"encoding/binary"
	"testing"
)

// authenticated runs a whole exchange and hands back the session it produced, which is the only
// way a session is ever made.
func authenticated(t *testing.T, user, workgroup string) *Session {
	t.Helper()

	srv := NewServer("SOMBRERO", "WORKGROUP", knownAccount())
	if err := authenticateAs(t, srv, user, workgroup, ntHashOf(testPassword)); err != nil {
		t.Fatalf("the exchange did not complete: %v", err)
	}

	return srv.Session()
}

// TestSecurityContextIsSettledByTheUser is what the whole of it rests on. The identifier a user is
// given is worked out from the name rather than stored, so it has to come out the same every time:
// it is what the client is told owns the files, and a user whose identifier moved between sessions
// would find that everything they had made belonged to somebody else.
func TestSecurityContextIsSettledByTheUser(t *testing.T) {
	first := authenticated(t, testUser, testWorkgroup).GetSecurityContext()
	second := authenticated(t, testUser, testWorkgroup).GetSecurityContext()

	if first.UserRID != second.UserRID {
		t.Errorf("the same user was given %d and then %d", first.UserRID, second.UserRID)
	}
	if first.User != second.User || first.Domain != second.Domain {
		t.Error("the same user came back under a different name")
	}

	firstSID, secondSID := first.DomainSID.SubAuthority, second.DomainSID.SubAuthority
	if len(firstSID) != len(secondSID) {
		t.Fatalf("the domain came out with %d parts and then %d", len(firstSID), len(secondSID))
	}
	for i := range firstSID {
		if firstSID[i] != secondSID[i] {
			t.Errorf("part %d of the domain came out %d and then %d", i, firstSID[i], secondSID[i])
		}
	}
}

// TestSecurityContextTellsUsersApart is the least it has to do. Two users sharing an identifier
// would be the same user as far as anything reading it is concerned.
func TestSecurityContextTellsUsersApart(t *testing.T) {
	seen := make(map[uint32]string)

	for _, user := range []string{"alice", "bob", "carol", "dave", "erin", "a", "ab", "ba"} {
		sc := authenticated(t, user, testWorkgroup).GetSecurityContext()

		if before, found := seen[sc.UserRID]; found {
			t.Errorf("%q and %q were both given %d", user, before, sc.UserRID)
		}
		seen[sc.UserRID] = user

		if sc.User != user {
			t.Errorf("the context came back naming %q, want %q", sc.User, user)
		}
	}
}

// TestSecurityContextFollowsTheWorkgroup is the same name in two workgroups, which are two
// different users. The workgroup goes into the identifier for exactly that reason.
func TestSecurityContextFollowsTheWorkgroup(t *testing.T) {
	here := authenticated(t, testUser, testWorkgroup).GetSecurityContext()
	there := authenticated(t, testUser, "a different workgroup entirely").GetSecurityContext()

	if here.UserRID == there.UserRID {
		t.Error("the same name in two workgroups was given the same identifier")
	}
}

// TestSecurityContextWithNoWorkgroup is the account that belongs to no workgroup, which takes the
// other of the two branches and builds a shorter identifier.
func TestSecurityContextWithNoWorkgroup(t *testing.T) {
	sc := authenticated(t, testUser, "").GetSecurityContext()

	if sc.User != testUser {
		t.Errorf("the context came back naming %q, want %q", sc.User, testUser)
	}
	if sc.Domain != "" {
		t.Errorf("a user in no workgroup was given the workgroup %q", sc.Domain)
	}
	if sc.UserRID == 0 {
		t.Error("the user was given an identifier of zero")
	}
}

// TestSecurityContextSaysHowLongItsIdentifierIs is the field a client reads before walking the
// parts behind it. A count that disagreed with what is actually there would have it read past the
// end of the identifier or stop short inside it.
func TestSecurityContextSaysHowLongItsIdentifierIs(t *testing.T) {
	for _, tt := range []struct {
		name      string
		workgroup string
	}{
		{"in a workgroup", testWorkgroup},
		{"in none", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sid := authenticated(t, testUser, tt.workgroup).GetSecurityContext().DomainSID

			if sid == nil {
				t.Fatal("no identifier came back at all")
			}
			if got, want := int(sid.SubAuthorityCount), len(sid.SubAuthority); got != want {
				t.Errorf("the identifier says it has %d parts and has %d", got, want)
			}
			if sid.Revision != 1 {
				t.Errorf("the identifier is of revision %d, want 1", sid.Revision)
			}
		})
	}
}

// TestSecurityContextOfNobody is the session with no user in it. Nothing should be built from it:
// an identifier handed out for a user that is not there is one a caller would go on to name in a
// response as though somebody owned it.
func TestSecurityContextOfNobody(t *testing.T) {
	var s Session

	sc := s.GetSecurityContext()
	if sc.User != "" || sc.Domain != "" || sc.UserRID != 0 || sc.DomainSID != nil {
		t.Errorf("a session with nobody in it gave the context %+v", sc)
	}
}

// TestSessionCarriesTheKeyThatWasAgreed is what everything above this layer signs and encrypts
// with. The key is derived from it for each purpose, so a session handing back nothing would have
// the whole of the protection above it built from an empty key.
func TestSessionCarriesTheKeyThatWasAgreed(t *testing.T) {
	ss := authenticated(t, testUser, testWorkgroup)

	if got := len(ss.SessionKey()); got != 16 {
		t.Errorf("the session key is %d bytes long, want 16", got)
	}
	if binary.LittleEndian.Uint64(ss.SessionKey()) == 0 && binary.LittleEndian.Uint64(ss.SessionKey()[8:]) == 0 {
		t.Error("the session key is nothing but zeroes")
	}

	if got := ss.User(); got != testUser {
		t.Errorf("the session is held by %q, want %q", got, testUser)
	}
	if got := ss.Domain(); got != testWorkgroup {
		t.Errorf("the session is in workgroup %q, want %q", got, testWorkgroup)
	}

	// What the session hands back is what the context carries, since the two are read by
	// different callers and have to agree.
	if sc := ss.GetSecurityContext(); string(sc.SessionKey) != string(ss.SessionKey()) {
		t.Error("the key on the context is not the key on the session")
	}
}

// TestSessionKeysAreAllDifferent is the four keys a session ends up holding. They are worked out
// from the one secret and told apart only by the constant that goes in with them, so any two
// coming out alike would mean what is signed under one could be forged under another.
func TestSessionKeysAreAllDifferent(t *testing.T) {
	ss := authenticated(t, testUser, testWorkgroup)

	keys := map[string][]byte{
		"the signing key of the client": ss.clientSigningKey,
		"the signing key of the server": ss.serverSigningKey,
		"the sealing key of the client": sealKey(ss.negotiateFlags, ss.exportedSessionKey, true),
		"the sealing key of the server": sealKey(ss.negotiateFlags, ss.exportedSessionKey, false),
	}

	seen := make(map[string]string)
	for name, key := range keys {
		if len(key) == 0 {
			t.Errorf("%s is empty", name)
			continue
		}
		if before, found := seen[string(key)]; found {
			t.Errorf("%s is the same as %s", name, before)
		}
		seen[string(key)] = name
	}
}
