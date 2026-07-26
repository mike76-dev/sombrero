package stores

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.sia.tech/core/types"
	"lukechampine.com/frand"
)

// sliceLength is the size of every metadata entry planted by plantObject.
const sliceLength = 10

// newSlabTestFixture registers a share and an account that owns the objects
// planted by plantObject.
func newSlabTestFixture(t *testing.T, db *Database) (Account, string) {
	t.Helper()

	u := uuid.New()
	if err := db.AddWorkgroup(Workgroup{UUID: u}); err != nil {
		t.Fatalf("AddWorkgroup: %v", err)
	}

	if err := db.AddAccount(Account{
		Username:  "alice",
		Password:  "secret123",
		Workgroup: u.String(),
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	acc, err := db.FindAccount("alice", u.String())
	if err != nil {
		t.Fatalf("FindAccount: %v", err)
	}

	sh := Share{
		Name:         "testshare",
		Type:         "indexd",
		ServerName:   "test-server",
		DataShards:   1,
		ParityShards: 0,
	}
	if err := db.RegisterShare(sh); err != nil {
		t.Fatalf("RegisterShare: %v", err)
	}

	return acc, sh.Name
}

// plantObject inserts a visible object whose metadata references the given slab
// keys, one entry per key. Passing the same key to more than one call makes the
// files share a slab, as they will once slabs are packed. Any directory in the
// path has to exist already.
func plantObject(t *testing.T, db *Database, share string, acc Account, path string, keys ...types.Hash256) {
	t.Helper()

	path = normalizePath(path)
	dir, name := splitPath(path)

	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		// The join on directories yields no row for a file in the root
		// folder, leaving directory_id NULL as the schema expects.
		const insertObject = `
			INSERT INTO objects (
				share_name,
				directory_id,
				name,
				full_path,
				size,
				account,
				workgroup,
				temporary
			)
			SELECT $1, d.id, $2, $3, $4, a.id, a.workgroup, FALSE
			FROM accounts a
			LEFT JOIN directories d
				ON d.share_name = $1
				AND d.full_path = $6
			WHERE a.id = $5
			RETURNING id
		`

		var oid uint64
		if err := tx.QueryRow(ctx, insertObject, share, name, path, len(keys)*sliceLength, acc.ID, dir).Scan(&oid); err != nil {
			return err
		}

		const insertMetadata = `
			INSERT INTO metadata (object_id, obj_offset, slab_key, data_offset, data_length)
			VALUES ($1, $2, $3, 0, $4)
		`

		for i, key := range keys {
			if _, err := tx.Exec(ctx, insertMetadata, oid, i*sliceLength, key[:], sliceLength); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("plant object %s: %v", path, err)
	}
}

// plantBufferedFile uploads a file of the given size that stays in a buffer,
// and returns its contents. Unless inFlight is set, the upload is finalized,
// which is what makes its buffer eligible for packing.
func plantBufferedFile(t *testing.T, db *Database, share string, acc Account, path string, size int, inFlight bool) []byte {
	t.Helper()

	data := make([]byte, size)
	frand.Read(data)

	uploadID, err := db.CreateUpload(acc, share, path)
	if err != nil {
		t.Fatalf("CreateUpload(%s): %v", path, err)
	}
	if err := db.AddBufferedSlab(uploadID, 0, data); err != nil {
		t.Fatalf("AddBufferedSlab(%s): %v", path, err)
	}
	if !inFlight {
		if err := db.FinalizeUpload(uploadID); err != nil {
			t.Fatalf("FinalizeUpload(%s): %v", path, err)
		}
	}

	return data
}

// pendingJobs returns the number of entries left in the upload queue.
func pendingJobs(t *testing.T, db *Database) int {
	t.Helper()

	var n int
	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM upload_jobs`).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count upload jobs: %v", err)
	}

	return n
}

// storedBuffers returns the number of buffers held in the database.
func storedBuffers(t *testing.T, db *Database) int {
	t.Helper()

	var n int
	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM buffers`).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count buffers: %v", err)
	}

	return n
}

// bufferedBytes returns the number of bytes held in buffers.
func bufferedBytes(t *testing.T, db *Database) int {
	t.Helper()

	var n int
	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(SUM(OCTET_LENGTH(data)), 0) FROM buffers`).Scan(&n)
	})
	if err != nil {
		t.Fatalf("sum buffered bytes: %v", err)
	}

	return n
}

// assertSlabs compares the slab keys reported by a delete against the expected
// set, ignoring their order.
func assertSlabs(t *testing.T, what string, got, want []types.Hash256) {
	t.Helper()

	sortKeys := func(keys []types.Hash256) {
		sort.Slice(keys, func(i, j int) bool {
			return keys[i].String() < keys[j].String()
		})
	}
	sortKeys(got)
	sortKeys(want)

	if len(got) != len(want) {
		t.Fatalf("%s: want %d unreferenced slabs %v, got %d %v", what, len(want), want, len(got), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: want unreferenced slabs %v, got %v", what, want, got)
		}
	}
}

// TestDeleteFileSharedSlab verifies that deleting a file only reports the slabs
// that no other file references any more. A slab shared with a surviving file
// must stay pinned, or deleting one small file would destroy the data of every
// other file packed into the same slab.
func TestDeleteFileSharedSlab(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share := newSlabTestFixture(t, db)

	shared := types.Hash256{1}
	sole := types.Hash256{2}

	plantObject(t, db, share, acc, "a.txt", shared)
	plantObject(t, db, share, acc, "b.txt", shared, sole)

	// The first delete leaves the shared slab referenced by b.txt.
	slabs, err := db.DeleteFile(acc, share, "a.txt")
	if err != nil {
		t.Fatalf("DeleteFile(a.txt): %v", err)
	}
	assertSlabs(t, "DeleteFile(a.txt)", slabs, nil)

	// The last reference is gone, so both of b.txt's slabs can be unpinned.
	slabs, err = db.DeleteFile(acc, share, "b.txt")
	if err != nil {
		t.Fatalf("DeleteFile(b.txt): %v", err)
	}
	assertSlabs(t, "DeleteFile(b.txt)", slabs, []types.Hash256{shared, sole})
}

// TestDeleteDirectorySharedSlab verifies that deleting a directory keeps a slab
// pinned while a file outside of it still references that slab, and reports it
// once that file is deleted too.
func TestDeleteDirectorySharedSlab(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share := newSlabTestFixture(t, db)

	if err := db.CreateDirectory(acc, share, "docs", true, false); err != nil {
		t.Fatalf("CreateDirectory: %v", err)
	}

	shared := types.Hash256{1}
	inner := types.Hash256{2}

	plantObject(t, db, share, acc, "docs/inside.txt", shared, inner)
	plantObject(t, db, share, acc, "outside.txt", shared)

	slabs, err := db.DeleteDirectory(acc, share, "docs")
	if err != nil {
		t.Fatalf("DeleteDirectory(docs): %v", err)
	}
	assertSlabs(t, "DeleteDirectory(docs)", slabs, []types.Hash256{inner})

	slabs, err = db.DeleteFile(acc, share, "outside.txt")
	if err != nil {
		t.Fatalf("DeleteFile(outside.txt): %v", err)
	}
	assertSlabs(t, "DeleteFile(outside.txt)", slabs, []types.Hash256{shared})
}

// slabSize is the size of a slab in the packing tests. The real one is
// dataShards * SectorSize, which is too large to move around in a test.
const slabSize = 1000

// TestClaimPackedSlabIncomplete verifies that nothing is claimed while the
// pending buffers do not add up to a full slab, so that no slab is ever paid
// for before it is worth uploading.
func TestClaimPackedSlabIncomplete(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share := newSlabTestFixture(t, db)

	if _, err := db.ClaimPackedSlab(share, slabSize, 0, 0); !errors.Is(err, ErrNoUploadJobs) {
		t.Fatalf("ClaimPackedSlab on an empty share: want %v, got %v", ErrNoUploadJobs, err)
	}

	plantBufferedFile(t, db, share, acc, "a.txt", 400, false)
	plantBufferedFile(t, db, share, acc, "b.txt", 400, false)

	if _, err := db.ClaimPackedSlab(share, slabSize, 0, 0); !errors.Is(err, ErrNoUploadJobs) {
		t.Fatalf("ClaimPackedSlab below a full slab: want %v, got %v", ErrNoUploadJobs, err)
	}
	if n := pendingJobs(t, db); n != 2 {
		t.Fatalf("want 2 jobs left in the queue, got %d", n)
	}
}

// TestClaimPackedSlabFull verifies that a claim takes just enough buffers to
// fill a slab, hands back their data in packing order, and leaves the rest of
// the queue alone.
func TestClaimPackedSlabFull(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share := newSlabTestFixture(t, db)

	a := plantBufferedFile(t, db, share, acc, "a.txt", 400, false)
	b := plantBufferedFile(t, db, share, acc, "b.txt", 400, false)
	c := plantBufferedFile(t, db, share, acc, "c.txt", 400, false)
	plantBufferedFile(t, db, share, acc, "d.txt", 400, false)

	jobs, err := db.ClaimPackedSlab(share, slabSize, 0, 0)
	if err != nil {
		t.Fatalf("ClaimPackedSlab: %v", err)
	}

	// The first two buffers fall short of a slab, so the third is claimed as
	// well and overshoots the boundary.
	if len(jobs) != 3 {
		t.Fatalf("want 3 claimed buffers, got %d", len(jobs))
	}

	var total uint64
	packed := make([]byte, 0, slabSize)
	for _, job := range jobs {
		total += job.DataLength
		packed = append(packed, job.Data...)
	}
	if total < slabSize {
		t.Fatalf("claimed %d bytes, want at least %d", total, slabSize)
	}

	if want := append(append(append([]byte{}, a...), b...), c...); !bytes.Equal(packed, want) {
		t.Fatalf("claimed data does not match the buffered contents")
	}

	// Only the claimed entries leave the queue.
	if n := pendingJobs(t, db); n != 1 {
		t.Fatalf("want 1 job left in the queue, got %d", n)
	}

	// The remaining buffer alone cannot fill a slab.
	if _, err := db.ClaimPackedSlab(share, slabSize, 0, 0); !errors.Is(err, ErrNoUploadJobs) {
		t.Fatalf("second ClaimPackedSlab: want %v, got %v", ErrNoUploadJobs, err)
	}
}

// TestClaimPackedSlabManySmallPieces verifies that a slab fills up no matter
// how many pieces it takes, since the claim is bounded by data size rather
// than by a row count. A backlog of tiny buffers used to stall forever once
// their number exceeded a fixed per-claim limit.
func TestClaimPackedSlabManySmallPieces(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share := newSlabTestFixture(t, db)

	// 400 pieces of 3 bytes each: filling a slab takes 334 of them, far more
	// than the 256 rows the claim used to be capped at.
	const pieces = 400
	for i := range pieces {
		plantBufferedFile(t, db, share, acc, fmt.Sprintf("tiny%03d.txt", i), 3, false)
	}

	jobs, err := db.ClaimPackedSlab(share, slabSize, 0, 0)
	if err != nil {
		t.Fatalf("ClaimPackedSlab: %v", err)
	}

	var total uint64
	for _, job := range jobs {
		total += job.DataLength
	}
	if total < slabSize {
		t.Fatalf("claimed %d bytes, want at least %d", total, slabSize)
	}
	if want := 334; len(jobs) != want {
		t.Fatalf("want %d claimed buffers, got %d", want, len(jobs))
	}

	// The rest falls short of another slab and stays queued.
	if n := pendingJobs(t, db); n != pieces-len(jobs) {
		t.Fatalf("want %d jobs left in the queue, got %d", pieces-len(jobs), n)
	}
	if _, err := db.ClaimPackedSlab(share, slabSize, 0, 0); !errors.Is(err, ErrNoUploadJobs) {
		t.Fatalf("second ClaimPackedSlab: want %v, got %v", ErrNoUploadJobs, err)
	}
}

// TestClaimPackedSlabSkipsIneligible verifies that buffers of uploads that are
// still in flight, buffers of other shares, and buffers that already fill a
// slab on their own are all left out of a packed slab.
func TestClaimPackedSlabSkipsIneligible(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share := newSlabTestFixture(t, db)

	other := Share{
		Name:         "othershare",
		Type:         "indexd",
		ServerName:   "test-server",
		DataShards:   1,
		ParityShards: 0,
	}
	if err := db.RegisterShare(other); err != nil {
		t.Fatalf("RegisterShare(other): %v", err)
	}

	// None of these may be claimed, even though they add up to several slabs.
	plantBufferedFile(t, db, share, acc, "inflight.txt", 600, true)
	plantBufferedFile(t, db, share, acc, "whole.txt", slabSize, false)
	plantBufferedFile(t, db, other.Name, acc, "elsewhere.txt", 600, false)

	if _, err := db.ClaimPackedSlab(share, slabSize, 0, 0); !errors.Is(err, ErrNoUploadJobs) {
		t.Fatalf("ClaimPackedSlab with only ineligible buffers: want %v, got %v", ErrNoUploadJobs, err)
	}

	// Adding an eligible pair of buffers claims those and nothing else. The
	// contents are compared rather than the sizes, because an ineligible
	// buffer of the same size would pass a size check unnoticed.
	a := plantBufferedFile(t, db, share, acc, "a.txt", 600, false)
	b := plantBufferedFile(t, db, share, acc, "b.txt", 600, false)

	jobs, err := db.ClaimPackedSlab(share, slabSize, 0, 0)
	if err != nil {
		t.Fatalf("ClaimPackedSlab: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("want 2 claimed buffers, got %d", len(jobs))
	}

	packed := make([]byte, 0, slabSize)
	for _, job := range jobs {
		packed = append(packed, job.Data...)
	}
	if want := append(append([]byte{}, a...), b...); !bytes.Equal(packed, want) {
		t.Fatalf("claimed buffers other than the eligible pair")
	}
}

// TestClaimPackedSlabRequeue verifies that a claimed batch can be put back,
// which is what happens when the upload of a packed slab fails.
func TestClaimPackedSlabRequeue(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share := newSlabTestFixture(t, db)

	plantBufferedFile(t, db, share, acc, "a.txt", 600, false)
	plantBufferedFile(t, db, share, acc, "b.txt", 600, false)

	jobs, err := db.ClaimPackedSlab(share, slabSize, 0, 0)
	if err != nil {
		t.Fatalf("ClaimPackedSlab: %v", err)
	}
	if n := pendingJobs(t, db); n != 0 {
		t.Fatalf("want an empty queue after the claim, got %d jobs", n)
	}

	for _, job := range jobs {
		if err := db.RequeueUploadJob(job.UploadID, job.MetadataID); err != nil {
			t.Fatalf("RequeueUploadJob: %v", err)
		}
	}

	if n := pendingJobs(t, db); n != len(jobs) {
		t.Fatalf("want %d jobs back in the queue, got %d", len(jobs), n)
	}

	again, err := db.ClaimPackedSlab(share, slabSize, 0, 0)
	if err != nil {
		t.Fatalf("ClaimPackedSlab after requeue: %v", err)
	}
	if len(again) != len(jobs) {
		t.Fatalf("want %d buffers reclaimed, got %d", len(jobs), len(again))
	}
	for i := range again {
		if again[i].MetadataID != jobs[i].MetadataID || !bytes.Equal(again[i].Data, jobs[i].Data) {
			t.Fatalf("reclaimed batch differs from the original claim")
		}
	}
}

// packSlab concatenates a claimed batch into the slab that would be uploaded,
// trimming the last item to the slab boundary the way the packer does.
func packSlab(t *testing.T, jobs []UploadJob, size uint64) []byte {
	t.Helper()

	slab := make([]byte, 0, size)
	for i := range jobs {
		if uint64(len(slab)+len(jobs[i].Data)) > size {
			jobs[i].Data = jobs[i].Data[:size-uint64(len(slab))]
		}
		slab = append(slab, jobs[i].Data...)
	}

	return slab
}

// readPacked reassembles a file from its metadata, taking the slab-backed
// slices from the given slab contents and the buffered ones from the database.
func readPacked(t *testing.T, db *Database, acc Account, share, path string, size uint64, slab []byte) []byte {
	t.Helper()

	slices, err := db.GetMetadata(acc, share, path, 0, size)
	if err != nil {
		t.Fatalf("GetMetadata(%s): %v", path, err)
	}

	out := make([]byte, 0, size)
	for _, s := range slices {
		if (s.Key == types.Hash256{}) {
			out = append(out, s.Data...)
			continue
		}
		if s.Offset+s.Length > uint64(len(slab)) {
			t.Fatalf("%s: slice [%d,%d) reaches past the %d byte slab", path, s.Offset, s.Offset+s.Length, len(slab))
		}
		out = append(out, slab[s.Offset:s.Offset+s.Length]...)
	}

	return out
}

// TestCompletePackedSlab verifies that a packed slab's items end up addressing
// their own stretch of the uploaded slab, so that every file still reads back
// exactly as it was written.
func TestCompletePackedSlab(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share := newSlabTestFixture(t, db)

	// The three buffers add up to exactly one slab, so nothing is split.
	want := map[string][]byte{
		"a.txt": plantBufferedFile(t, db, share, acc, "a.txt", 300, false),
		"b.txt": plantBufferedFile(t, db, share, acc, "b.txt", 300, false),
		"c.txt": plantBufferedFile(t, db, share, acc, "c.txt", 400, false),
	}

	jobs, err := db.ClaimPackedSlab(share, slabSize, 0, 0)
	if err != nil {
		t.Fatalf("ClaimPackedSlab: %v", err)
	}
	slab := packSlab(t, jobs, slabSize)
	if len(slab) != slabSize {
		t.Fatalf("want a %d byte slab, got %d", slabSize, len(slab))
	}

	key := types.Hash256{9}
	if err := db.CompletePackedSlab(jobs, key); err != nil {
		t.Fatalf("CompletePackedSlab: %v", err)
	}

	for path, content := range want {
		if got := readPacked(t, db, acc, share, path, uint64(len(content)), slab); !bytes.Equal(got, content) {
			t.Fatalf("%s reads back wrong after packing", path)
		}
	}

	// Every buffer made it into the slab, so none may be left behind.
	if n := pendingJobs(t, db); n != 0 {
		t.Fatalf("want an empty queue, got %d jobs", n)
	}
	if n := storedBuffers(t, db); n != 0 {
		t.Fatalf("want no buffers left, got %d", n)
	}
}

// TestCompletePackedSlabSplit verifies that the part of the last buffer that
// does not fit into the slab is kept as a buffer of its own and queued for the
// next one, while the file reads back as a whole.
func TestCompletePackedSlabSplit(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share := newSlabTestFixture(t, db)

	a := plantBufferedFile(t, db, share, acc, "a.txt", 600, false)
	b := plantBufferedFile(t, db, share, acc, "b.txt", 600, false)

	jobs, err := db.ClaimPackedSlab(share, slabSize, 0, 0)
	if err != nil {
		t.Fatalf("ClaimPackedSlab: %v", err)
	}
	slab := packSlab(t, jobs, slabSize)

	key := types.Hash256{9}
	if err := db.CompletePackedSlab(jobs, key); err != nil {
		t.Fatalf("CompletePackedSlab: %v", err)
	}

	// a.txt fits entirely, b.txt is split after its first 400 bytes.
	if got := readPacked(t, db, acc, share, "a.txt", uint64(len(a)), slab); !bytes.Equal(got, a) {
		t.Fatalf("a.txt reads back wrong after packing")
	}
	if got := readPacked(t, db, acc, share, "b.txt", uint64(len(b)), slab); !bytes.Equal(got, b) {
		t.Fatalf("b.txt reads back wrong after being split")
	}

	// The remainder is queued, and holds only the bytes that did not fit.
	if n := pendingJobs(t, db); n != 1 {
		t.Fatalf("want the remainder queued, got %d jobs", n)
	}
	if n := storedBuffers(t, db); n != 1 {
		t.Fatalf("want 1 buffer left, got %d", n)
	}
	if n := bufferedBytes(t, db); n != len(b)-400 {
		t.Fatalf("want %d buffered bytes left, got %d", len(b)-400, n)
	}

	// The remainder alone is not enough for another slab.
	if _, err := db.ClaimPackedSlab(share, slabSize, 0, 0); !errors.Is(err, ErrNoUploadJobs) {
		t.Fatalf("ClaimPackedSlab after the split: want %v, got %v", ErrNoUploadJobs, err)
	}
}

// TestCompletePackedSlabDeletedFile verifies that a file deleted while its slab
// was being uploaded does not take the rest of the slab down with it, and that
// the slab is only reported as unneeded once every one of its files is gone.
func TestCompletePackedSlabDeletedFile(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share := newSlabTestFixture(t, db)

	plantBufferedFile(t, db, share, acc, "a.txt", 500, false)
	b := plantBufferedFile(t, db, share, acc, "b.txt", 500, false)

	jobs, err := db.ClaimPackedSlab(share, slabSize, 0, 0)
	if err != nil {
		t.Fatalf("ClaimPackedSlab: %v", err)
	}
	slab := packSlab(t, jobs, slabSize)

	// a.txt goes away while the slab is in flight.
	if _, err := db.DeleteFile(acc, share, "a.txt"); err != nil {
		t.Fatalf("DeleteFile(a.txt): %v", err)
	}

	key := types.Hash256{9}
	if err := db.CompletePackedSlab(jobs, key); err != nil {
		t.Fatalf("CompletePackedSlab: %v", err)
	}
	if got := readPacked(t, db, acc, share, "b.txt", uint64(len(b)), slab); !bytes.Equal(got, b) {
		t.Fatalf("b.txt reads back wrong after a.txt was deleted")
	}

	// With every file of the batch gone, the caller has to be told that the
	// slab it uploaded is not needed after all.
	plantBufferedFile(t, db, share, acc, "c.txt", 500, false)
	plantBufferedFile(t, db, share, acc, "d.txt", 500, false)

	jobs, err = db.ClaimPackedSlab(share, slabSize, 0, 0)
	if err != nil {
		t.Fatalf("ClaimPackedSlab: %v", err)
	}
	packSlab(t, jobs, slabSize)

	if _, err := db.DeleteFile(acc, share, "c.txt"); err != nil {
		t.Fatalf("DeleteFile(c.txt): %v", err)
	}
	if _, err := db.DeleteFile(acc, share, "d.txt"); err != nil {
		t.Fatalf("DeleteFile(d.txt): %v", err)
	}

	if err := db.CompletePackedSlab(jobs, types.Hash256{8}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CompletePackedSlab with every file gone: want %v, got %v", ErrNotFound, err)
	}
}

// TestCompletePackedSlabRetry verifies that completing the same batch twice is
// harmless, so that a retry after a partial failure cannot duplicate the
// remainder of a split buffer.
func TestCompletePackedSlabRetry(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share := newSlabTestFixture(t, db)

	a := plantBufferedFile(t, db, share, acc, "a.txt", 600, false)
	b := plantBufferedFile(t, db, share, acc, "b.txt", 600, false)

	jobs, err := db.ClaimPackedSlab(share, slabSize, 0, 0)
	if err != nil {
		t.Fatalf("ClaimPackedSlab: %v", err)
	}
	slab := packSlab(t, jobs, slabSize)

	key := types.Hash256{9}
	if err := db.CompletePackedSlab(jobs, key); err != nil {
		t.Fatalf("CompletePackedSlab: %v", err)
	}
	if err := db.CompletePackedSlab(jobs, key); err != nil {
		t.Fatalf("CompletePackedSlab again: %v", err)
	}

	if got := readPacked(t, db, acc, share, "a.txt", uint64(len(a)), slab); !bytes.Equal(got, a) {
		t.Fatalf("a.txt reads back wrong after a repeated completion")
	}
	if got := readPacked(t, db, acc, share, "b.txt", uint64(len(b)), slab); !bytes.Equal(got, b) {
		t.Fatalf("b.txt reads back wrong after a repeated completion")
	}
	if n := pendingJobs(t, db); n != 1 {
		t.Fatalf("want the remainder queued once, got %d jobs", n)
	}
	if n := storedBuffers(t, db); n != 1 {
		t.Fatalf("want 1 buffer left, got %d", n)
	}
}

// backdateJobs makes every queued job look as if it had been waiting for the
// given duration, so that the age trigger can be tested without waiting.
func backdateJobs(t *testing.T, db *Database, age time.Duration) {
	t.Helper()

	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE upload_jobs
			SET created_at = NOW() - MAKE_INTERVAL(secs => $1::DOUBLE PRECISION)
		`, age.Seconds())
		return err
	})
	if err != nil {
		t.Fatalf("backdate upload jobs: %v", err)
	}
}

// TestClaimPackedSlabAgeTrigger verifies that leftover data which has waited
// longer than the configured age is claimed even though it falls short of a
// slab, and that it keeps waiting while the age is not configured.
func TestClaimPackedSlabAgeTrigger(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share := newSlabTestFixture(t, db)

	a := plantBufferedFile(t, db, share, acc, "a.txt", 300, false)
	b := plantBufferedFile(t, db, share, acc, "b.txt", 300, false)
	backdateJobs(t, db, 48*time.Hour)

	// However long it has been waiting, without an age it waits on.
	if _, err := db.ClaimPackedSlab(share, slabSize, 0, 0); !errors.Is(err, ErrNoUploadJobs) {
		t.Fatalf("ClaimPackedSlab without an age: want %v, got %v", ErrNoUploadJobs, err)
	}

	// Not old enough yet.
	if _, err := db.ClaimPackedSlab(share, slabSize, 0, 72*time.Hour); !errors.Is(err, ErrNoUploadJobs) {
		t.Fatalf("ClaimPackedSlab below the age: want %v, got %v", ErrNoUploadJobs, err)
	}

	jobs, err := db.ClaimPackedSlab(share, slabSize, 0, 24*time.Hour)
	if err != nil {
		t.Fatalf("ClaimPackedSlab past the age: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("want both buffers claimed, got %d", len(jobs))
	}

	packed := make([]byte, 0, slabSize)
	for _, job := range jobs {
		packed = append(packed, job.Data...)
	}
	if want := append(append([]byte{}, a...), b...); !bytes.Equal(packed, want) {
		t.Fatalf("claimed data does not match the buffered contents")
	}

	// An incomplete slab is claimed as a whole, so nothing may be left over.
	if n := pendingJobs(t, db); n != 0 {
		t.Fatalf("want an empty queue, got %d jobs", n)
	}
}

// TestClaimPackedSlabMinSize verifies that the age trigger holds back until the
// leftover data is worth a slab, since an incomplete slab costs as much as a
// full one.
func TestClaimPackedSlabMinSize(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share := newSlabTestFixture(t, db)

	plantBufferedFile(t, db, share, acc, "a.txt", 300, false)
	backdateJobs(t, db, 48*time.Hour)

	// Old enough, but not yet worth uploading.
	if _, err := db.ClaimPackedSlab(share, slabSize, 500, 24*time.Hour); !errors.Is(err, ErrNoUploadJobs) {
		t.Fatalf("ClaimPackedSlab below the minimum size: want %v, got %v", ErrNoUploadJobs, err)
	}

	// A second file takes the leftover data past the minimum.
	plantBufferedFile(t, db, share, acc, "b.txt", 300, false)
	backdateJobs(t, db, 48*time.Hour)

	jobs, err := db.ClaimPackedSlab(share, slabSize, 500, 24*time.Hour)
	if err != nil {
		t.Fatalf("ClaimPackedSlab past the minimum size: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("want both buffers claimed, got %d", len(jobs))
	}
}

// TestClaimPackedSlabAgePrefersFullSlab verifies that a full slab is still
// claimed as exactly one slab once the age has passed, rather than dragging in
// everything that is waiting.
func TestClaimPackedSlabAgePrefersFullSlab(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share := newSlabTestFixture(t, db)

	for _, name := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		plantBufferedFile(t, db, share, acc, name, 400, false)
	}
	backdateJobs(t, db, 48*time.Hour)

	jobs, err := db.ClaimPackedSlab(share, slabSize, 0, 24*time.Hour)
	if err != nil {
		t.Fatalf("ClaimPackedSlab: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("want 3 buffers claimed for one slab, got %d", len(jobs))
	}
	if n := pendingJobs(t, db); n != 1 {
		t.Fatalf("want 1 job left in the queue, got %d", n)
	}
}
