package transfer

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"

	"go.sia.tech/core/types"
	"go.sia.tech/indexd/slabs"
)

// The flags that say which of the ways to the bytes a part carries.
const (
	flagObject uint8 = 1 << iota
	flagPin
	flagSource
	flagInline
)

// EncodeTo implements types.EncoderTo.
func (h Header) EncodeTo(e *types.Encoder) {
	e.WriteTime(h.CreatedAt)
	e.WriteString(h.Source)
	e.WriteString(h.Origin)
	e.WriteString(h.Share)
	e.WriteString(h.Workgroup)
}

// readTime reads a time as the instant it is, in UTC, so that a description
// reads the same wherever it is read.
func readTime(d *types.Decoder) time.Time {
	return d.ReadTime().UTC()
}

// DecodeFrom implements types.DecoderFrom.
func (h *Header) DecodeFrom(d *types.Decoder) {
	h.CreatedAt = readTime(d)
	h.Source = d.ReadString()
	h.Origin = d.ReadString()
	h.Share = d.ReadString()
	h.Workgroup = d.ReadString()
}

// EncodeTo implements types.EncoderTo.
func (c Connection) EncodeTo(e *types.Encoder) {
	c.Share.EncodeTo(e)
	c.Workgroup.EncodeTo(e)
	types.EncodeSlice(e, c.Accounts)
	types.EncodeSlice(e, c.Policies)
	e.WriteBytes(c.AppKey)
}

// DecodeFrom implements types.DecoderFrom.
func (c *Connection) DecodeFrom(d *types.Decoder) {
	c.Share.DecodeFrom(d)
	c.Workgroup.DecodeFrom(d)
	types.DecodeSlice(d, &c.Accounts)
	types.DecodeSlice(d, &c.Policies)
	c.AppKey = d.ReadBytes()
}

// EncodeTo implements types.EncoderTo.
func (s Share) EncodeTo(e *types.Encoder) {
	e.WriteString(s.Name)
	e.WriteString(s.Type)
	e.WriteString(s.Server)
	e.WriteString(s.Password)
	e.WriteString(s.Bucket)
	e.WriteString(s.Remark)
	e.WriteTime(s.CreatedAt)
	e.WriteUint8(s.DataShards)
	e.WriteUint8(s.ParityShards)
	e.WriteBool(s.AllowGuest)
	e.WriteBool(s.AllowAnonymous)
	e.WriteString(s.PublicDir)
}

// DecodeFrom implements types.DecoderFrom.
func (s *Share) DecodeFrom(d *types.Decoder) {
	s.Name = d.ReadString()
	s.Type = d.ReadString()
	s.Server = d.ReadString()
	s.Password = d.ReadString()
	s.Bucket = d.ReadString()
	s.Remark = d.ReadString()
	s.CreatedAt = readTime(d)
	s.DataShards = d.ReadUint8()
	s.ParityShards = d.ReadUint8()
	s.AllowGuest = d.ReadBool()
	s.AllowAnonymous = d.ReadBool()
	s.PublicDir = d.ReadString()
}

// EncodeTo implements types.EncoderTo.
func (w Workgroup) EncodeTo(e *types.Encoder) {
	e.Write(w.UUID[:])
	e.WriteString(w.Name)
	types.EncodeSlice(e, w.PublicDirs)
}

// DecodeFrom implements types.DecoderFrom.
func (w *Workgroup) DecodeFrom(d *types.Decoder) {
	d.Read(w.UUID[:])
	w.Name = d.ReadString()
	types.DecodeSlice(d, &w.PublicDirs)
}

// EncodeTo implements types.EncoderTo.
func (p PublicDir) EncodeTo(e *types.Encoder) {
	e.WriteString(p.Path)
	e.WriteBool(p.ReadOnly)
	e.WriteBool(p.CaseSensitive)
}

// DecodeFrom implements types.DecoderFrom.
func (p *PublicDir) DecodeFrom(d *types.Decoder) {
	p.Path = d.ReadString()
	p.ReadOnly = d.ReadBool()
	p.CaseSensitive = d.ReadBool()
}

// EncodeTo implements types.EncoderTo.
func (a Account) EncodeTo(e *types.Encoder) {
	e.WriteString(a.Name)
	e.WriteBytes(a.PasswordHash)
	e.WriteTime(a.CreatedAt)
}

// DecodeFrom implements types.DecoderFrom.
func (a *Account) DecodeFrom(d *types.Decoder) {
	a.Name = d.ReadString()
	a.PasswordHash = d.ReadBytes()
	a.CreatedAt = readTime(d)
}

// EncodeTo implements types.EncoderTo.
func (p Policy) EncodeTo(e *types.Encoder) {
	e.WriteString(p.Account)
	e.WriteBool(p.Read)
	e.WriteBool(p.Write)
	e.WriteBool(p.Delete)
	e.WriteBool(p.Execute)
}

// DecodeFrom implements types.DecoderFrom.
func (p *Policy) DecodeFrom(d *types.Decoder) {
	p.Account = d.ReadString()
	p.Read = d.ReadBool()
	p.Write = d.ReadBool()
	p.Delete = d.ReadBool()
	p.Execute = d.ReadBool()
}

// EncodeTo implements types.EncoderTo.
func (dir Directory) EncodeTo(e *types.Encoder) {
	e.WriteString(dir.Path)
	e.WriteString(dir.Owner)
	e.WriteBool(dir.Private)
	e.WriteBool(dir.ReadOnly)
	e.WriteTime(dir.CreatedAt)
	e.WriteTime(dir.ModifiedAt)
}

// DecodeFrom implements types.DecoderFrom.
func (dir *Directory) DecodeFrom(d *types.Decoder) {
	dir.Path = d.ReadString()
	dir.Owner = d.ReadString()
	dir.Private = d.ReadBool()
	dir.ReadOnly = d.ReadBool()
	dir.CreatedAt = readTime(d)
	dir.ModifiedAt = readTime(d)
}

// EncodeTo implements types.EncoderTo.
func (f File) EncodeTo(e *types.Encoder) {
	e.WriteString(f.Path)
	e.WriteString(f.Owner)
	e.WriteUint64(f.Size)
	e.WriteTime(f.CreatedAt)
	e.WriteTime(f.ModifiedAt)
	types.EncodeSlice(e, f.Parts)
}

// DecodeFrom implements types.DecoderFrom.
func (f *File) DecodeFrom(d *types.Decoder) {
	f.Path = d.ReadString()
	f.Owner = d.ReadString()
	f.Size = d.ReadUint64()
	f.CreatedAt = readTime(d)
	f.ModifiedAt = readTime(d)
	types.DecodeSlice(d, &f.Parts)
}

// EncodeTo implements types.EncoderTo.
func (p Part) EncodeTo(e *types.Encoder) {
	e.WriteUint64(p.Offset)
	e.WriteUint64(p.DataOffset)
	e.WriteUint64(p.Length)

	var flags uint8
	if p.Object != (types.Hash256{}) {
		flags |= flagObject
	}
	if p.Pin != nil {
		flags |= flagPin
	}
	if p.Source != nil {
		flags |= flagSource
	}
	if p.Inline != nil {
		flags |= flagInline
	}
	e.WriteUint8(flags)

	if flags&flagObject != 0 {
		p.Object.EncodeTo(e)
	}
	if flags&flagPin != 0 {
		p.Pin.EncodeTo(e)
	}
	if flags&flagSource != 0 {
		p.Source.EncodeTo(e)
	}
	if flags&flagInline != 0 {
		e.WriteBytes(p.Inline)
	}
}

// DecodeFrom implements types.DecoderFrom.
func (p *Part) DecodeFrom(d *types.Decoder) {
	p.Offset = d.ReadUint64()
	p.DataOffset = d.ReadUint64()
	p.Length = d.ReadUint64()

	flags := d.ReadUint8()
	if flags&flagObject != 0 {
		p.Object.DecodeFrom(d)
	}
	if flags&flagPin != 0 {
		p.Pin = new(Pin)
		p.Pin.DecodeFrom(d)
	}
	if flags&flagSource != 0 {
		p.Source = new(Source)
		p.Source.DecodeFrom(d)
	}
	if flags&flagInline != 0 {
		p.Inline = d.ReadBytes()
	}
}

// EncodeTo implements types.EncoderTo.
func (p Pin) EncodeTo(e *types.Encoder) {
	e.Write(p.DataKey[:])
	types.EncodeSlice(e, p.Slabs)
}

// DecodeFrom implements types.DecoderFrom.
func (p *Pin) DecodeFrom(d *types.Decoder) {
	d.Read(p.DataKey[:])
	types.DecodeSlice[slabs.SlabSlice](d, &p.Slabs)
}

// EncodeTo implements types.EncoderTo.
func (s Source) EncodeTo(e *types.Encoder) {
	e.WriteString(s.Kind)
	e.WriteString(s.Origin)
	e.WriteString(s.Bucket)
	e.WriteString(s.Key)
}

// DecodeFrom implements types.DecoderFrom.
func (s *Source) DecodeFrom(d *types.Decoder) {
	s.Kind = d.ReadString()
	s.Origin = d.ReadString()
	s.Bucket = d.ReadString()
	s.Key = d.ReadString()
}

// A Writer writes a description as a stream, so that what is being described
// never has to be held whole. Close finishes the stream and must be called.
type Writer struct {
	w    io.Writer
	hash hash.Hash

	dirs      uint64
	files     uint64
	connected bool
	err       error
	done      bool
}

// NewWriter opens a stream and writes the header, which says where what follows
// came from.
func NewWriter(w io.Writer, header Header) (*Writer, error) {
	tw := &Writer{w: w, hash: sha256.New()}

	var buf bytes.Buffer
	e := types.NewEncoder(&buf)
	e.WriteString(magic)
	e.WriteUint8(Version)
	e.Flush()
	if err := tw.write(buf.Bytes()); err != nil {
		return nil, err
	}

	if err := tw.record(kindHeader, header); err != nil {
		return nil, err
	}

	return tw, nil
}

// Connection writes what the folders and files belong to, which makes the stream
// a catalog. It comes once, before any of them.
func (w *Writer) Connection(c Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if w.connected {
		return errors.New("the catalog has its connection already")
	}
	if w.dirs > 0 || w.files > 0 {
		return errors.New("the connection has to come before the folders and files")
	}
	if err := w.record(kindConnection, c); err != nil {
		return err
	}
	w.connected = true

	return nil
}

// Directory writes a folder.
func (w *Writer) Directory(dir Directory) error {
	if err := dir.Validate(); err != nil {
		return err
	}
	if err := w.record(kindDirectory, dir); err != nil {
		return err
	}
	w.dirs++

	return nil
}

// File writes a file and the parts it is made of.
func (w *Writer) File(f File) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if err := w.record(kindFile, f); err != nil {
		return err
	}
	w.files++

	return nil
}

// Close ends the stream with what it holds and the digest of everything before
// it, which is what a reader checks the stream against.
func (w *Writer) Close() error {
	if w.err != nil {
		return w.err
	}
	if w.done {
		return errors.New("the transfer is closed already")
	}
	w.done = true

	// The end record is the one thing the digest does not cover, since it is
	// what carries the digest.
	var payload bytes.Buffer
	e := types.NewEncoder(&payload)
	e.WriteUint64(w.dirs)
	e.WriteUint64(w.files)
	e.Write(w.hash.Sum(nil))
	e.Flush()

	var buf bytes.Buffer
	frame := types.NewEncoder(&buf)
	frame.WriteUint8(kindEnd)
	frame.WriteUint64(uint64(payload.Len()))
	frame.Write(payload.Bytes())
	frame.Flush()

	_, err := w.w.Write(buf.Bytes())

	return err
}

// record writes one length-prefixed record, so that a reader can step over the
// kinds it does not know.
func (w *Writer) record(kind uint8, v types.EncoderTo) error {
	if w.err != nil {
		return w.err
	}
	if w.done {
		return errors.New("the transfer is closed already")
	}

	var payload bytes.Buffer
	e := types.NewEncoder(&payload)
	v.EncodeTo(e)
	if err := e.Flush(); err != nil {
		return w.fail(err)
	}
	if payload.Len() > maxRecordSize {
		return w.fail(fmt.Errorf("the record of %d byte(s) is larger than the %d a reader takes", payload.Len(), maxRecordSize))
	}

	var buf bytes.Buffer
	frame := types.NewEncoder(&buf)
	frame.WriteUint8(kind)
	frame.WriteUint64(uint64(payload.Len()))
	frame.Write(payload.Bytes())
	if err := frame.Flush(); err != nil {
		return w.fail(err)
	}

	return w.write(buf.Bytes())
}

// write puts the bytes on the stream and into the digest.
func (w *Writer) write(p []byte) error {
	if w.err != nil {
		return w.err
	}
	if _, err := w.w.Write(p); err != nil {
		return w.fail(err)
	}
	w.hash.Write(p)

	return nil
}

// fail keeps the first error, since what follows it is not worth reporting.
func (w *Writer) fail(err error) error {
	if w.err == nil {
		w.err = err
	}

	return w.err
}

// A Reader reads a description as a stream, in the order it was written and with
// the times it carries in UTC. A whole stream ends with io.EOF.
type Reader struct {
	r    io.Reader
	hash hash.Hash

	header     Header
	connection *Connection
	dirs       uint64
	files      uint64
	done       bool

	// pending is the record read while looking for the connection, which
	// turned out to be the first folder or file instead.
	pending *record
}

// record is one record of the stream as it was read, to be decoded later.
type record struct {
	kind    uint8
	payload []byte
}

// NewReader opens a stream and reads its header, refusing what it is not meant
// to read before any of it is believed.
func NewReader(r io.Reader) (*Reader, error) {
	tr := &Reader{r: r, hash: sha256.New()}

	head := make([]byte, 8+len(magic)+1)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, ErrNotATransfer
	}
	tr.hash.Write(head)

	d := types.NewDecoder(io.LimitedReader{R: bytes.NewReader(head), N: int64(len(head))})
	if d.ReadString() != magic {
		return nil, ErrNotATransfer
	}
	if version := d.ReadUint8(); version > Version {
		return nil, fmt.Errorf("%w: %d, this reader knows %d", ErrUnsupportedVersion, version, Version)
	}
	if err := d.Err(); err != nil {
		return nil, ErrNotATransfer
	}

	kind, payload, err := tr.next()
	if err != nil {
		return nil, err
	}
	if kind != kindHeader {
		return nil, fmt.Errorf("%w: it does not start with a header", ErrNotATransfer)
	}
	if err := decode(payload, &tr.header); err != nil {
		return nil, err
	}

	// A catalog says what it belongs to before anything it holds; a plain
	// description goes straight on to the folders and files.
	kind, payload, err = tr.next()
	if err != nil {
		return nil, err
	}
	if kind == kindConnection {
		tr.connection = new(Connection)
		if err := decode(payload, tr.connection); err != nil {
			return nil, err
		}
	} else {
		tr.pending = &record{kind: kind, payload: payload}
	}

	return tr, nil
}

// Header returns what the stream says about where it came from.
func (r *Reader) Header() Header {
	return r.header
}

// Connection returns what the folders and files belong to, which only a catalog
// says: it is nil for a plain description.
func (r *Reader) Connection() *Connection {
	return r.connection
}

// Counts returns how many folders and files the stream said it holds, which is
// only known once it has been read to the end.
func (r *Reader) Counts() (dirs, files uint64) {
	return r.dirs, r.files
}

// Next returns the next folder or file. One of them is set; both are nil at the
// end of the stream, which Next reports as io.EOF.
func (r *Reader) Next() (*Directory, *File, error) {
	for {
		if r.done {
			return nil, nil, io.EOF
		}

		var kind uint8
		var payload []byte
		if r.pending != nil {
			kind, payload = r.pending.kind, r.pending.payload
			r.pending = nil
		} else {
			var err error
			if kind, payload, err = r.next(); err != nil {
				return nil, nil, err
			}
		}

		switch kind {
		case kindConnection:
			return nil, nil, errors.New("the connection comes before the folders and files, not among them")

		case kindEnd:
			if err := r.end(payload); err != nil {
				return nil, nil, err
			}
			return nil, nil, io.EOF

		case kindDirectory:
			var dir Directory
			if err := decode(payload, &dir); err != nil {
				return nil, nil, err
			}
			return &dir, nil, nil

		case kindFile:
			var f File
			if err := decode(payload, &f); err != nil {
				return nil, nil, err
			}
			return nil, &f, nil
		}

		// A kind this reader does not know was written by a newer one, and the
		// length is there so that it can be stepped over rather than guessed at.
	}
}

// next reads one record off the stream. The end record is left out of the
// digest, since it is what carries it.
func (r *Reader) next() (kind uint8, payload []byte, err error) {
	var head [9]byte
	if _, err := io.ReadFull(r.r, head[:]); err != nil {
		return 0, nil, ErrTruncated
	}

	d := types.NewDecoder(io.LimitedReader{R: bytes.NewReader(head[:]), N: int64(len(head))})
	kind = d.ReadUint8()
	length := d.ReadUint64()
	if length > maxRecordSize {
		return 0, nil, fmt.Errorf("the record of %d byte(s) is larger than the %d this reader takes", length, maxRecordSize)
	}

	payload = make([]byte, length)
	if _, err := io.ReadFull(r.r, payload); err != nil {
		return 0, nil, ErrTruncated
	}

	if kind != kindEnd {
		r.hash.Write(head[:])
		r.hash.Write(payload)
	}

	return kind, payload, nil
}

// end reads what the stream ends with and checks it against what was read.
func (r *Reader) end(payload []byte) error {
	d := types.NewDecoder(io.LimitedReader{R: bytes.NewReader(payload), N: int64(len(payload))})
	r.dirs = d.ReadUint64()
	r.files = d.ReadUint64()
	digest := make([]byte, sha256.Size)
	d.Read(digest)
	if err := d.Err(); err != nil {
		return ErrTruncated
	}
	if !bytes.Equal(digest, r.hash.Sum(nil)) {
		return ErrCorrupted
	}
	r.done = true

	return nil
}

// decode reads one record's payload, which is whole or nothing.
func decode(payload []byte, v types.DecoderFrom) error {
	d := types.NewDecoder(io.LimitedReader{R: bytes.NewReader(payload), N: int64(len(payload))})
	v.DecodeFrom(d)

	return d.Err()
}
