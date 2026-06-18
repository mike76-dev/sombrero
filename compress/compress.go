package compress

import (
	"bytes"
	"compress/flate"
	"io"

	"github.com/mike76-dev/sombrero/compress/lz77"
	"github.com/mike76-dev/sombrero/compress/lznt1"
	"github.com/mike76-dev/sombrero/smb2"
	"github.com/pierrec/lz4/v4"
)

// Compressor performs compression and decompression of data.
type Compressor struct {
	algorithm uint16
}

// New returns an initialized Compressor.
func New(algo uint16) *Compressor {
	return &Compressor{algo}
}

// Compress compresses the provided input.
func (c *Compressor) Compress(src []byte) ([]byte, error) {
	switch c.algorithm {
	case smb2.COMPRESSION_LZ77:
		return lz77.Compress(src), nil

	case smb2.COMPRESSION_LZ77_HUFFMAN:
		var buf bytes.Buffer
		w, err := flate.NewWriter(&buf, flate.DefaultCompression)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(src); err != nil {
			w.Close()
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil

	case smb2.COMPRESSION_LZ4:
		var buf bytes.Buffer
		w := lz4.NewWriter(&buf)
		if _, err := w.Write(src); err != nil {
			w.Close()
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil

	case smb2.COMPRESSION_LZNT1:
		return lznt1.Compress(src), nil

	default:
		return nil, nil
	}
}

// Decompress decompresses the provided input.
func (c *Compressor) Decompress(src []byte, limit int) ([]byte, error) {
	switch c.algorithm {
	case smb2.COMPRESSION_LZ77:
		return lz77.Decompress(src, limit)

	case smb2.COMPRESSION_LZ77_HUFFMAN:
		r := flate.NewReader(bytes.NewReader(src))
		defer r.Close()
		dst, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		return dst, nil

	case smb2.COMPRESSION_LZ4:
		r := lz4.NewReader(bytes.NewReader(src))
		dst, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		return dst, nil

	case smb2.COMPRESSION_LZNT1:
		return lznt1.Decompress(src)

	default:
		return nil, nil
	}
}
