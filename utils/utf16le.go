package utils

import (
	"bytes"
	"encoding/binary"
	"unicode/utf16"
)

// EncodedStringLen returns the length of an UTF-16-encoded string in bytes.
func EncodedStringLen(s string) int {
	l := 0
	for _, r := range s {
		if 0x10000 <= r && r <= '\U0010FFFF' {
			l += 4
		} else {
			l += 2
		}
	}
	return l
}

// EncodeString encodes a string in the UTF-16LE format.
func EncodeString(dst []byte, src string) int {
	ws := utf16.Encode([]rune(src))
	for i, w := range ws {
		binary.LittleEndian.PutUint16(dst[2*i:2*i+2], w)
	}
	return len(ws) * 2
}

// EncodeStringToBytes encodes a string in the UTF-16LE format; the result is returned.
func EncodeStringToBytes(s string) []byte {
	if len(s) == 0 {
		return nil
	}
	ws := utf16.Encode([]rune(s))
	bs := make([]byte, len(ws)*2)
	for i, w := range ws {
		binary.LittleEndian.PutUint16(bs[2*i:2*i+2], w)
	}
	return bs
}

// DecodeToString decodes an UTF-16LE-encoded string.
func DecodeToString(bs []byte) string {
	if len(bs) == 0 {
		return ""
	}
	ws := make([]uint16, len(bs)/2)
	for i := range ws {
		ws[i] = binary.LittleEndian.Uint16(bs[2*i : 2*i+2])
	}
	if len(ws) > 0 && ws[len(ws)-1] == 0 {
		ws = ws[:len(ws)-1]
	}
	return string(utf16.Decode(ws))
}

// NullTerminatedToStrings converts a sequence of null-terminated Unicode strings to a slice of Golang strings.
func NullTerminatedToStrings(b []byte) []string {
	var result []string
	for len(b) > 0 {
		// Anything below the printable range marks the start of an entry rather than belonging
		// to one, and is stepped over. The step is taken whatever is left, including on the last
		// byte: stopping short of it there would be to come round to the same byte again having
		// consumed nothing.
		if b[0] < 32 {
			b = b[1:]
			continue
		}

		i := bytes.IndexByte(b, 0)
		if i < 0 {
			// The last string was cut off before the byte that ends it. What is there is taken
			// as it stands and the walk ends. Leaving it to be looked at again would be the
			// same loop that never comes back, and these bytes are read off the wire before
			// anybody has said who they are.
			result = append(result, string(b))
			break
		}

		result = append(result, string(b[:i]))
		b = b[i+1:]
	}

	return result
}
