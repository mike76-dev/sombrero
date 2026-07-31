package lznt1

import (
	"testing"

	"github.com/mike76-dev/sombrero/compress/internal/comptest"
)

func TestRoundTrip(t *testing.T) {
	comptest.Run(t, comptest.Codec{
		Compress: Compress,

		// LZNT1 carries the length of each chunk in its header, so it has no use for the one the
		// round trip hands it.
		Decompress: func(src []byte, _ int) ([]byte, error) { return Decompress(src) },
	})
}
