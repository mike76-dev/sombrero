package stores

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"testing"

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

	if _, err := db.ClaimPackedSlab(share, slabSize); !errors.Is(err, ErrNoUploadJobs) {
		t.Fatalf("ClaimPackedSlab on an empty share: want %v, got %v", ErrNoUploadJobs, err)
	}

	plantBufferedFile(t, db, share, acc, "a.txt", 400, false)
	plantBufferedFile(t, db, share, acc, "b.txt", 400, false)

	if _, err := db.ClaimPackedSlab(share, slabSize); !errors.Is(err, ErrNoUploadJobs) {
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

	jobs, err := db.ClaimPackedSlab(share, slabSize)
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
	if _, err := db.ClaimPackedSlab(share, slabSize); !errors.Is(err, ErrNoUploadJobs) {
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

	if _, err := db.ClaimPackedSlab(share, slabSize); !errors.Is(err, ErrNoUploadJobs) {
		t.Fatalf("ClaimPackedSlab with only ineligible buffers: want %v, got %v", ErrNoUploadJobs, err)
	}

	// Adding an eligible pair of buffers claims those and nothing else. The
	// contents are compared rather than the sizes, because an ineligible
	// buffer of the same size would pass a size check unnoticed.
	a := plantBufferedFile(t, db, share, acc, "a.txt", 600, false)
	b := plantBufferedFile(t, db, share, acc, "b.txt", 600, false)

	jobs, err := db.ClaimPackedSlab(share, slabSize)
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

	jobs, err := db.ClaimPackedSlab(share, slabSize)
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

	again, err := db.ClaimPackedSlab(share, slabSize)
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
