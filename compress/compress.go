package compress

import (
	"errors"

	"github.com/mike76-dev/sombrero/compress/huffman"
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
		return huffman.Compress(src), nil

	case smb2.COMPRESSION_LZ4:
		// A bare LZ4 block, not a frame. The frame format leads with a magic number and a header
		// carrying the uncompressed size, all of which the SMB2 compression transform header
		// already says: OriginalCompressedSegmentSize is that size, and the algorithm is named in
		// the header beside it. A frame here would be a second, redundant header that the peer is
		// not looking for.
		if len(src) == 0 {
			return nil, nil
		}

		// Sizing the destination at the bound is what makes the compression always succeed, so
		// there is no incompressible case to answer for here.
		dst := make([]byte, lz4.CompressBlockBound(len(src)))
		var c lz4.Compressor
		n, err := c.CompressBlock(src, dst)
		if err != nil {
			return nil, err
		}
		return dst[:n], nil

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
		return huffman.Decompress(src, limit)

	case smb2.COMPRESSION_LZ4:
		// A bare block, so nothing in the data says how big it decompresses to. The caller knows
		// it: the transform header carries the uncompressed size, and that is the limit.
		if len(src) == 0 {
			return nil, nil
		}
		if limit <= 0 {
			return nil, errors.New("compress: LZ4 needs the uncompressed size")
		}

		dst := make([]byte, limit)
		n, err := lz4.UncompressBlock(src, dst)
		if err != nil {
			return nil, err
		}
		return dst[:n], nil

	case smb2.COMPRESSION_LZNT1:
		return lznt1.Decompress(src)

	default:
		return nil, nil
	}
}
