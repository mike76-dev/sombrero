package client

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/core/types"
	"go.sia.tech/indexd/api/app"
	"go.sia.tech/indexd/slabs"
)

// A tag is written in the binary layout below and read in that or in the JSON
// of version 1, which begins with '{' where the binary begins with its version.
//
//	byte 0     tagBinary
//	byte 1     tagCompressed where the rest is deflated, 0 where it is not
//	then       omitted; the shares by name; the pieces, each as the index of
//	           its share, its path, offset, the distance of its start from the
//	           end of the piece before it, length and size; all as varints
//	last 4     CRC-32 of the above, taken before compression
const (
	tagJSON       = 1
	tagBinary     = 2
	tagCompressed = 1
)

// A tag may come from anyone's account, so a reader holds it to these bounds.
const (
	maxTagPlain  = 1 << 20
	maxTagPieces = 1 << 16
	maxTagPath   = 1 << 12
)

// sealedOverhead is what sealing adds to a tag: the nonce and the authentication
// tag of the XChaCha20-Poly1305 the SDK seals metadata with.
const sealedOverhead = 24 + 16

// maxTagSize is the most a tag may encode to. The indexer caps an object's sealed
// metadata, so a tag names what fits and counts the rest.
const maxTagSize = slabs.MaxMetadataSize - sealedOverhead

// objectTag is what an object carries about its own contents: the runs of the
// files whose bytes are in it, and where in it they are.
//
// The indexer stores it sealed with the account's app key, so it is of no use to
// anyone else and of every use to whoever holds the account: an account that was
// all an object store becomes one that can say what its objects were files of.
//
// It is also small: an object packed with many small files cannot name them all,
// so it names the first of them and says how many it left out.
type objectTag struct {
	Version int           `json:"sombrero"`
	Pieces  []objectPiece `json:"pieces"`
	Omitted int           `json:"omitted,omitempty"`
}

// encode returns the tag as the indexer will take it: all of its pieces where
// they fit, else the first of them, the rest counted in Omitted. It is nil for a
// tag that could name nothing, which leaves the object saying nothing about
// itself.
func (tag objectTag) encode() json.RawMessage {
	if len(tag.Pieces) == 0 {
		return nil
	}

	// A catalog is what a rescue of the account starts from, so its pieces go
	// first, where a tag that cannot hold everything still holds them.
	tag.Pieces = slices.Clone(tag.Pieces)
	sort.SliceStable(tag.Pieces, func(i, j int) bool {
		return isCatalog(tag.Pieces[i]) && !isCatalog(tag.Pieces[j])
	})
	fits := func(n int) bool { return len(tag.pack(n)) <= maxTagSize }

	n := len(tag.Pieces)
	if !fits(n) {
		// The pieces lie in object order, so the ones that fit are the first.
		// Compression keeps the size from growing strictly with their number,
		// so the search is checked, and stepped either way where it has to be.
		n = sort.Search(n, func(i int) bool { return !fits(i + 1) })
		for n > 0 && !fits(n) {
			n--
		}
		for n < len(tag.Pieces) && fits(n+1) {
			n++
		}
	}
	if n == 0 {
		return nil
	}

	return tag.pack(n)
}

// isCatalog reports whether the piece is of a catalog the share keeps of itself.
func isCatalog(piece objectPiece) bool {
	return strings.HasPrefix(piece.Path, transfer.CatalogFolder+"/")
}

// pack lays out the first n pieces, the rest counted as omitted, and returns the
// smaller of the plain and the compressed form.
func (tag objectTag) pack(n int) []byte {
	body := tag.body(n)
	plain := append([]byte{tagBinary, 0}, body...)

	var zipped bytes.Buffer
	zipped.Write([]byte{tagBinary, tagCompressed})
	w, _ := flate.NewWriter(&zipped, flate.BestCompression)
	_, _ = w.Write(body)
	_ = w.Close()
	if zipped.Len() < len(plain) {
		return zipped.Bytes()
	}

	return plain
}

// body is the layout of the first n pieces, ending with their checksum.
func (tag objectTag) body(n int) []byte {
	var shares []string
	index := make(map[string]int)
	for _, piece := range tag.Pieces[:n] {
		if _, ok := index[piece.Share]; !ok {
			index[piece.Share] = len(shares)
			shares = append(shares, piece.Share)
		}
	}

	b := binary.AppendUvarint(nil, uint64(len(tag.Pieces)-n))
	b = binary.AppendUvarint(b, uint64(len(shares)))
	for _, share := range shares {
		b = appendString(b, share)
	}

	b = binary.AppendUvarint(b, uint64(n))
	var end uint64
	for _, piece := range tag.Pieces[:n] {
		b = binary.AppendUvarint(b, uint64(index[piece.Share]))
		b = appendString(b, piece.Path)
		b = binary.AppendUvarint(b, piece.Offset)
		b = binary.AppendVarint(b, int64(piece.At)-int64(end))
		b = binary.AppendUvarint(b, piece.Length)
		b = binary.AppendUvarint(b, piece.Size)
		end = piece.At + piece.Length
	}

	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(b))
}

// appendString appends a string as its length and its bytes.
func appendString(b []byte, s string) []byte {
	return append(binary.AppendUvarint(b, uint64(len(s))), s...)
}

// decodeTag reads a tag in the binary layout, and reports whether it was one:
// anything that does not decode within the bounds, or does not check, is not.
func decodeTag(meta []byte) (objectTag, bool) {
	if len(meta) < 2 || meta[0] != tagBinary {
		return objectTag{}, false
	}

	body := meta[2:]
	if meta[1]&tagCompressed != 0 {
		r := flate.NewReader(bytes.NewReader(body))
		inflated, err := io.ReadAll(io.LimitReader(r, maxTagPlain+1))
		_ = r.Close()
		if err != nil || len(inflated) > maxTagPlain {
			return objectTag{}, false
		}
		body = inflated
	}
	if len(body) < 4 {
		return objectTag{}, false
	}
	sum, body := binary.BigEndian.Uint32(body[len(body)-4:]), body[:len(body)-4]
	if crc32.ChecksumIEEE(body) != sum {
		return objectTag{}, false
	}

	r := bytes.NewReader(body)
	omitted, err := binary.ReadUvarint(r)
	if err != nil || omitted > maxTagPlain {
		return objectTag{}, false
	}
	shares, ok := readStrings(r, maxTagPieces, maxTagPath)
	if !ok {
		return objectTag{}, false
	}

	n, err := binary.ReadUvarint(r)
	if err != nil || n == 0 || n > maxTagPieces {
		return objectTag{}, false
	}
	tag := objectTag{Version: tagBinary, Omitted: int(omitted), Pieces: make([]objectPiece, 0, n)}
	var end uint64
	for range n {
		share, err := binary.ReadUvarint(r)
		if err != nil || share >= uint64(len(shares)) {
			return objectTag{}, false
		}
		path, ok := readString(r, maxTagPath)
		if !ok {
			return objectTag{}, false
		}
		offset, err1 := binary.ReadUvarint(r)
		distance, err2 := binary.ReadVarint(r)
		length, err3 := binary.ReadUvarint(r)
		size, err4 := binary.ReadUvarint(r)
		at := int64(end) + distance
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || at < 0 {
			return objectTag{}, false
		}

		tag.Pieces = append(tag.Pieces, objectPiece{Share: shares[share], Path: path, Offset: offset, At: uint64(at), Length: length, Size: size})
		end = uint64(at) + length
	}
	if r.Len() != 0 {
		return objectTag{}, false
	}

	return tag, true
}

// readStrings reads a counted list of strings within the given bounds.
func readStrings(r *bytes.Reader, maxCount, maxLen int) ([]string, bool) {
	n, err := binary.ReadUvarint(r)
	if err != nil || n > uint64(maxCount) {
		return nil, false
	}
	strs := make([]string, 0, n)
	for range n {
		s, ok := readString(r, maxLen)
		if !ok {
			return nil, false
		}
		strs = append(strs, s)
	}

	return strs, true
}

// readString reads a string of at most maxLen bytes, as appendString wrote it.
func readString(r *bytes.Reader, maxLen int) (string, bool) {
	n, err := binary.ReadUvarint(r)
	if err != nil || n > uint64(maxLen) || n > uint64(r.Len()) {
		return "", false
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", false
	}

	return string(b), true
}

// objectPiece is one run of a file's bytes inside an object.
type objectPiece struct {
	Share string `json:"share"`
	Path  string `json:"path"`

	// Offset is where the run begins in the file, At where it begins in the
	// object, and Length how long it is.
	Offset uint64 `json:"offset"`
	At     uint64 `json:"at"`
	Length uint64 `json:"length"`

	// Size is what the file measured when the run was written, which is a hint
	// rather than the truth: a file that was still being written measured less
	// then than it does now.
	Size uint64 `json:"size,omitempty"`
}

// tag returns what the object made of these jobs is to say about its contents.
// A lookup that fails costs the object its tag, not the upload: an object that
// says nothing about itself is what every one written before this was.
func (ic *IndexdClient) tag(jobs []stores.UploadJob) json.RawMessage {
	ids := make([]uint64, 0, len(jobs))
	for _, job := range jobs {
		ids = append(ids, job.MetadataID)
	}

	owners, err := ic.db.PieceOwners(ids)
	if err != nil {
		log.Printf("failed to look up the files of %d piece(s): %v", len(ids), err)
		return nil
	}

	return tagPieces(jobs, owners)
}

// tagPieces returns the tag for an object made of the given jobs, laid out in
// the order they were packed into it. The jobs of a file nobody could name are
// left out, which is what a file deleted while its piece waited leaves behind.
func tagPieces(jobs []stores.UploadJob, owners map[uint64]stores.PieceOwner) json.RawMessage {
	tag := objectTag{Version: tagBinary}

	var at uint64
	for _, job := range jobs {
		length := uint64(len(job.Data))
		if owner, ok := owners[job.MetadataID]; ok {
			tag.Pieces = append(tag.Pieces, objectPiece{
				Share:  owner.Share,
				Path:   owner.Path,
				Offset: job.ObjOffset,
				At:     at,
				Length: length,
				Size:   owner.Size,
			})
		}
		at += length
	}

	// A tag that cannot be encoded is no reason to leave the data unuploaded:
	// the object is then one that says nothing about itself, as every object
	// written before tagging does.
	return tag.encode()
}

// tagOfSlab returns what the object of the slab is to say about itself, worked
// out from the files that reference it now. It is nil for a slab no file does,
// which is one on its way to being unpinned rather than retagged.
func (ic *IndexdClient) tagOfSlab(key types.Hash256) (json.RawMessage, bool) {
	runs, err := ic.db.SlabRuns(ic.share, ic.workgroup, key)
	if err != nil {
		log.Printf("failed to look up the runs of slab %s: %v", key, err)
		return nil, false
	}
	if len(runs) == 0 {
		return nil, false
	}

	tag := objectTag{Version: tagBinary, Pieces: make([]objectPiece, 0, len(runs))}
	for _, run := range runs {
		tag.Pieces = append(tag.Pieces, objectPiece{
			Share:  run.Share,
			Path:   run.Path,
			Offset: run.ObjOffset,
			At:     run.DataOffset,
			Length: run.DataLength,
			Size:   run.Size,
		})
	}

	meta := tag.encode()
	if meta == nil {
		log.Printf("the tag of slab %s cannot name even one of its files", key)
		return nil, false
	}

	return meta, true
}

// retag has the objects of these slabs say what they hold now. It is for the
// changes that leave the data where it is and the names of it elsewhere: a file
// that was renamed or deleted out of a slab it shares with others.
//
// The work is handed to a worker, since a client waiting on a rename is not to
// wait on the indexer as well. A tag that is not written is a tag that is out of
// date, which costs a rescue the right name and nothing else.
func (ic *IndexdClient) retag(keys []types.Hash256) {
	if len(keys) == 0 {
		return
	}

	ic.mu.Lock()
	if ic.retagging == nil {
		ic.retagging = make(map[types.Hash256]struct{})
	}
	for _, key := range keys {
		ic.retagging[key] = struct{}{}
	}
	ic.mu.Unlock()

	select {
	case ic.retagChan <- struct{}{}:
	default:
	}
}

// tagSilentSlabs tells the slabs that say nothing about themselves, or say it in
// an older layout, what they hold: data written before tagging existed, or by an
// older server. It walks the account's object log once at startup, which costs a
// request per hundred objects, and queues a tag for every object that wants one.
// The worker then passes over what no file of this share references.
func (ic *IndexdClient) tagSilentSlabs(ctx context.Context) {
	src, ok := ic.backend.(AccountObjects)
	if !ok {
		return
	}

	pinned, err := listPinned(ctx, src)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("failed to look for untagged slabs in share %s: %v", ic.share, err)
		}
		return
	}

	var silent []types.Hash256
	for key, obj := range pinned {
		o := obj.Object
		if o == nil {
			fetched, err := src.Object(ctx, key)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				continue
			}
			o = &fetched
		}
		if wantsTag(o.Metadata()) {
			silent = append(silent, key)
		}
	}

	ic.retag(silent)
}

// wantsTag reports whether an object's metadata is due a rewrite at startup: it
// says nothing, or says it the way an older server did.
func wantsTag(meta json.RawMessage) bool {
	tag, ok := parseTag(meta)
	return !ok || tag.Version != tagBinary
}

// retagObjects is the worker that writes the tags the changes left owing. A tag
// is not worth holding a shutdown up for: what is left unwritten is a name a
// rescue would have got right, and the data is not waiting on it.
func (ic *IndexdClient) retagObjects(ctx context.Context) {
	var retry <-chan time.Time
	for {
		select {
		case <-ic.drainChan:
			return
		case <-ctx.Done():
			return
		case <-ic.retagChan:
		case <-retry:
		}

		// A backend that would not take the tags is waited out rather than
		// taken for an answer: the slabs stay on the list until it does.
		retry = nil
		if ic.writeTags(ctx) {
			retry = time.After(retryDelay)
		}
	}
}

// writeTags tells the objects of the waiting slabs what they hold, and reports
// whether any of them is still owed a tag.
func (ic *IndexdClient) writeTags(ctx context.Context) (owing bool) {
	for _, key := range ic.takeRetags() {
		select {
		case <-ic.drainChan:
			return false
		default:
		}

		if err := ctx.Err(); err != nil {
			return false
		}

		meta, owed := ic.tagOfSlab(key)
		if !owed {
			continue
		}
		if err := ic.backend.Retag(ctx, key, meta); err != nil {
			if refused(err) {
				// The indexer has made up its mind; asking again would only fill
				// the log. The object goes on saying what it said before.
				log.Printf("the indexer would not take the tag of slab %s: %v", key, err)
				continue
			}
			log.Printf("failed to tell slab %s what it holds, leaving it for a retry: %v", key, err)
			ic.retagLater(key)
			owing = true
		}
	}

	return owing
}

// refused reports whether the indexer turned a request down for what it was,
// rather than failing to answer it: an answer that waiting will not change.
func refused(err error) bool {
	var httpErr *app.HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}

	return httpErr.StatusCode >= 400 && httpErr.StatusCode < 500 && httpErr.StatusCode != http.StatusTooManyRequests
}

// takeRetags takes the slabs that are waiting off the list, so that one that is
// put back is tried in the next round rather than again in this one.
func (ic *IndexdClient) takeRetags() []types.Hash256 {
	ic.mu.Lock()
	defer ic.mu.Unlock()

	keys := make([]types.Hash256, 0, len(ic.retagging))
	for key := range ic.retagging {
		keys = append(keys, key)
	}
	ic.retagging = nil

	return keys
}

// retagLater puts a slab back on the list, for the round after this one.
func (ic *IndexdClient) retagLater(key types.Hash256) {
	ic.mu.Lock()
	defer ic.mu.Unlock()

	if ic.retagging == nil {
		ic.retagging = make(map[types.Hash256]struct{})
	}
	ic.retagging[key] = struct{}{}
}

// parseTag reads what an object says about its contents, in either layout, and
// reports whether it said anything this server understands.
func parseTag(meta json.RawMessage) (objectTag, bool) {
	if len(meta) == 0 {
		return objectTag{}, false
	}
	if meta[0] == tagBinary {
		return decodeTag(meta)
	}

	var tag objectTag
	if err := json.Unmarshal(meta, &tag); err != nil {
		return objectTag{}, false
	}
	if tag.Version != tagJSON || len(tag.Pieces) == 0 {
		return objectTag{}, false
	}

	return tag, true
}
