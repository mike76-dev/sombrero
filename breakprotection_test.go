package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// A break notification is the one message the server sends off its own back, so it is the one
// message whose protection is not decided by the request it answers. Two rules govern it, and
// they pull in opposite directions.
//
// It is not signed. 3.3.4.6 and 3.3.4.7 both say so outright - "The message SHOULD NOT be
// signed" - and the reason is that every signing rule in 3.3.4.1.1 is conditioned on the request
// having been signed by the client, which a notification has none of. A session that requires
// signing for everything else still gets its breaks unsigned.
//
// It is encrypted, when the session encrypts. 3.3.4.1.4 puts no such condition on it: everything
// but NEGOTIATE and SESSION_SETUP goes encrypted once Session.EncryptData is set.

func TestIntegrationOplockBreakIsNotSigned(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice").signing()
	alice.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)

	bob := h.dial("bob")
	go bob.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OVERWRITE)

	note := alice.recv(10 * time.Second)
	assertUnsigned(t, note, "oplock break")
}

func TestIntegrationLeaseBreakIsNotSigned(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice").signing()
	alice.createLeased("dir/file", aliceKey, rwh, 2, smb2.FILE_OPEN)

	bob := h.dial("bob")
	go bob.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OVERWRITE)

	note := alice.recv(10 * time.Second)
	if !isLeaseBreak(note) {
		t.Fatalf("what arrived was not a lease break")
	}
	assertUnsigned(t, note, "lease break")
}

// assertUnsigned holds a notification to what 3.3.4.6 asks for: no signed flag, and a signature
// field left at zero. The flag alone is not enough - a server that signed but forgot the flag
// would still be putting a signature on the wire.
func assertUnsigned(t *testing.T, note []byte, what string) {
	t.Helper()

	h := smb2.Header(note)
	if h.IsFlagSet(smb2.FLAGS_SIGNED) {
		t.Errorf("the %s is marked signed", what)
	}
	if sig := note[48:64]; !bytes.Equal(sig, make([]byte, 16)) {
		t.Errorf("the %s carries a signature % x, want none", what, sig)
	}
}

func TestIntegrationOplockBreakIsEncrypted(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice").encrypting()
	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)

	bob := h.dial("bob")
	go bob.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OVERWRITE)

	// What comes off the wire has to be unreadable, and what it comes apart into has to be the
	// break. Asserting only the first would be satisfied by the server sending rubbish.
	sealed := alice.recv(10 * time.Second)
	note := alice.decrypted(sealed)

	if cmd := smb2.Header(note).Command(); cmd != smb2.SMB2_OPLOCK_BREAK {
		t.Fatalf("the message under the encryption is command %#x, want an oplock break", cmd)
	}
	if fid := brokenFileID(note); !bytes.Equal(fid, createdFileID(held)) {
		t.Errorf("the break names % x, want alice's open % x", fid, createdFileID(held))
	}
}

func TestIntegrationLeaseBreakIsEncrypted(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice").encrypting()
	alice.createLeased("dir/file", aliceKey, rwh, 2, smb2.FILE_OPEN)

	bob := h.dial("bob")
	go bob.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OVERWRITE)

	sealed := alice.recv(10 * time.Second)
	note := alice.decrypted(sealed)

	if !isLeaseBreak(note) {
		t.Fatalf("the message under the encryption is not a lease break")
	}
	if key := brokenLeaseKey(note); key != aliceKey {
		t.Errorf("the break names lease key % x, want alice's % x", key, aliceKey)
	}
}
