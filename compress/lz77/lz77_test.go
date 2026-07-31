package lz77

import (
	"testing"

	"github.com/mike76-dev/sombrero/compress/internal/comptest"
)

func TestRoundTrip(t *testing.T) {
	comptest.Run(t, comptest.Codec{
		Compress:   Compress,
		Decompress: Decompress,
	})
}
