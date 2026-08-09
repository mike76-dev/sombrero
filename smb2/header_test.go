package smb2

import (
	"bytes"
	"testing"
)

// TestSetNonceFillsTheWholeField is the nonce that is shorter than the field it goes in. AES-CCM
// counts out eleven bytes and AES-GCM twelve, and the Nonce field of the transform header is sixteen
// for either of them, so what is left over has to be zeroed rather than left as it was found: the
// field travels in the clear as part of the associated data, and the far end reads back as many
// bytes of it as its own cipher takes.
func TestSetNonceFillsTheWholeField(t *testing.T) {
	for _, size := range []int{11, 12, 16} {
		// A header whose nonce field is already written over, which is what makes the difference
		// between filling the field and filling the front of it.
		h := Header(bytes.Repeat([]byte{0xff}, SMB2TransformHeaderSize))

		nonce := bytes.Repeat([]byte{'n'}, size)
		h.SetNonce(nonce)

		want := make([]byte, 16)
		copy(want, nonce)
		if got := h.Nonce(); !bytes.Equal(got, want) {
			t.Errorf("a nonce of %d bytes left the field as %x, want %x", size, got, want)
		}
	}
}
