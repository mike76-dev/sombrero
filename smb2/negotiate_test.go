package smb2

import "testing"

// TestDialectCapabilities is the table of what each dialect may carry, which is what a server
// narrows its own capabilities with before telling a client what it can do ([MS-SMB2] 3.3.5.4).
// Every capability is named for every dialect, so that a bit added to one of the sets has to be
// thought about here rather than quietly reaching a dialect that has no such thing.
func TestDialectCapabilities(t *testing.T) {
	const (
		leasing = GLOBAL_CAP_LEASING | GLOBAL_CAP_LARGE_MTU
		threeX  = GLOBAL_CAP_MULTI_CHANNEL | GLOBAL_CAP_PERSISTENT_HANDLES | GLOBAL_CAP_DIRECTORY_LEASING
	)

	for _, tt := range []struct {
		name    string
		dialect uint16
		want    uint32
	}{
		// 2.0.2 is the dialect that predates all of them; DFS is as old as SMB2 itself.
		{"2.0.2", SMB_DIALECT_202, GLOBAL_CAP_DFS},

		// Leases and the large MTU arrived with 2.1.
		{"2.1", SMB_DIALECT_21, GLOBAL_CAP_DFS | leasing},

		// 3.x brought channels, persistent handles and directory leases, and carries encryption
		// as a capability up to and including 3.0.2.
		{"3.0", SMB_DIALECT_30, GLOBAL_CAP_DFS | leasing | threeX | GLOBAL_CAP_ENCRYPTION},
		{"3.0.2", SMB_DIALECT_302, GLOBAL_CAP_DFS | leasing | threeX | GLOBAL_CAP_ENCRYPTION},

		// 3.1.1 settles a cipher in a negotiate context, so it is the one 3.x dialect that does
		// not carry the encryption capability.
		{"3.1.1", SMB_DIALECT_311, GLOBAL_CAP_DFS | leasing | threeX},

		// The wildcard answer to a legacy negotiate stands for "SMB2, dialect to be settled". It
		// is not 2.0.2, and the client negotiates again before anything is done over it.
		{"multi-credit", SMB_DIALECT_MULTICREDIT, GLOBAL_CAP_DFS | leasing},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := DialectCapabilities(tt.dialect); got != tt.want {
				t.Errorf("DialectCapabilities(%#x) = %#x, want %#x", tt.dialect, got, tt.want)
			}
		})
	}
}

// TestDialectCapabilitiesNarrow is the property the table is there for: a later dialect never has
// fewer capabilities than an earlier one, apart from the encryption flag that 3.1.1 gives up in
// favour of a negotiate context. A mask that lost a capability along the way would take it from
// every client that speaks the newer dialect.
func TestDialectCapabilitiesNarrow(t *testing.T) {
	older := DialectCapabilities(SMB_DIALECT_202)
	for _, dialect := range []uint16{SMB_DIALECT_21, SMB_DIALECT_30, SMB_DIALECT_302} {
		caps := DialectCapabilities(dialect)
		if caps&older != older {
			t.Errorf("dialect %#x carries %#x, which drops something the one before it had (%#x)", dialect, caps, older)
		}
		older = caps
	}

	// 3.1.1 keeps everything 3.0.2 had but the encryption flag.
	if got, want := DialectCapabilities(SMB_DIALECT_311), older&^GLOBAL_CAP_ENCRYPTION; got != want {
		t.Errorf("3.1.1 carries %#x, want everything 3.0.2 has but encryption (%#x)", got, want)
	}
}
