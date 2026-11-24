package client

import (
	"io"
	"sync/atomic"
)

// Transfer is what a client has moved to and from its storage since it
// started, in bytes of file data: what was handed to the network to store
// and what was fetched from it, before any redundancy the network adds.
type Transfer struct {
	Uploaded   uint64
	Downloaded uint64
}

// Meter is the part of a client that counts its transfers.
type Meter interface {
	Transferred() Transfer
}

// transferMeter counts transfers from any goroutine.
type transferMeter struct {
	uploaded   atomic.Uint64
	downloaded atomic.Uint64
}

func (m *transferMeter) Transferred() Transfer {
	return Transfer{Uploaded: m.uploaded.Load(), Downloaded: m.downloaded.Load()}
}

// countingReader adds what is read through it to the counter.
type countingReader struct {
	r io.Reader
	n *atomic.Uint64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(uint64(n))
	return n, err
}

// countingWriter adds what is written through it to the counter.
type countingWriter struct {
	w io.Writer
	n *atomic.Uint64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(uint64(n))
	return n, err
}
