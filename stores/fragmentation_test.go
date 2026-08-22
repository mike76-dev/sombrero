package stores

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.sia.tech/core/types"
	"lukechampine.com/frand"
)

// plantPiece inserts a visible file whose single metadata entry occupies the
// given slice of the slab, as a packed upload leaves it.
func plantPiece(t *testing.T, db *Database, share string, acc Account, path string, key types.Hash256, offset, length uint64) {
	t.Helper()

	path = normalizePath(path)
	dir, name := splitPath(path)

	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const insertObject = `
			INSERT INTO objects (share_name, directory_id, name, full_path, size, account, workgroup, temporary)
			SELECT $1, d.id, $2, $3, $4, a.id, a.workgroup, FALSE
			FROM accounts a
			LEFT JOIN directories d
				ON d.share_name = $1
				AND d.full_path = $6
			WHERE a.id = $5
			RETURNING id
		`

		var oid uint64
		if err := tx.QueryRow(ctx, insertObject, share, name, path, int64(length), acc.ID, dir).Scan(&oid); err != nil {
			return err
		}

		const insertMetadata = `
			INSERT INTO metadata (object_id, obj_offset, slab_key, data_offset, data_length)
			VALUES ($1, 0, $2, $3, $4)
		`

		_, err := tx.Exec(ctx, insertMetadata, oid, key[:], int64(offset), int64(length))
		return err
	})
	if err != nil {
		t.Fatalf("plant piece %s: %v", path, err)
	}
}

// plantHole fills a slab with two pieces and deletes the first, which leaves a
// hole of the given size at the front and the rest of the slab in use.
func plantHole(t *testing.T, db *Database, share string, acc Account, name string, key types.Hash256, hole uint64) {
	t.Helper()

	plantPiece(t, db, share, acc, name+"-gone.txt", key, 0, hole)
	plantPiece(t, db, share, acc, name+"-kept.txt", key, hole, slabSize-hole)

	if _, err := db.DeleteFile(acc, share, name+"-gone.txt"); err != nil {
		t.Fatalf("DeleteFile(%s): %v", name, err)
	}
}

// assertPacked checks the slabs a listing reported against the expected key,
// used and filled bytes and piece count, in order.
func assertPacked(t *testing.T, got []PackedSlab, want []PackedSlab) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("want %d packed slab(s), got %d: %+v", len(want), len(got), got)
	}
	for i := range want {
		if got[i].Key != want[i].Key || got[i].Used != want[i].Used ||
			got[i].Filled != want[i].Filled || got[i].Pieces != want[i].Pieces {
			t.Fatalf("slab %d: want %+v, got %+v", i, want[i], got[i])
		}
		if got[i].Size != slabSize {
			t.Fatalf("slab %d: want size %d, got %d", i, slabSize, got[i].Size)
		}
	}
}

// TestPackedSlabs verifies that only the holes files leave behind count as dead
// space: a slab nobody has touched holds none, whether it was filled to the
// brim or left half empty by an upload that aged out.
func TestPackedSlabs(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share, wg := newSlabTestFixture(t, db)

	// A slab packed out of three pieces that fill it.
	packed := types.Hash256{1}
	plantPiece(t, db, share, acc, "a.txt", packed, 0, 500)
	plantPiece(t, db, share, acc, "b.txt", packed, 500, 300)
	plantPiece(t, db, share, acc, "c.txt", packed, 800, 200)

	// A slab uploaded whole, and one left half empty by an aged upload.
	plantPiece(t, db, share, acc, "big.txt", types.Hash256{2}, 0, slabSize)
	plantPiece(t, db, share, acc, "d.txt", types.Hash256{3}, 0, 400)

	slabs, err := db.PackedSlabs(share, wg, slabSize, 0)
	if err != nil {
		t.Fatalf("PackedSlabs: %v", err)
	}
	if len(slabs) != 0 {
		t.Fatalf("want nothing reported before anything is deleted, got %+v", slabs)
	}

	// Deleting the middle file leaves a hole of its size behind.
	if _, err := db.DeleteFile(acc, share, "b.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	slabs, err = db.PackedSlabs(share, wg, slabSize, 0)
	if err != nil {
		t.Fatalf("PackedSlabs: %v", err)
	}
	assertPacked(t, slabs, []PackedSlab{
		{Key: packed, Used: 700, Filled: 1000, Pieces: 2},
	})
	if f := slabs[0].Fragmentation(); f != 0.3 {
		t.Fatalf("want the emptied slab 30%% fragmented, got %v", f)
	}
	if w := slabs[0].Wasted(); w != 300 {
		t.Fatalf("want 300 bytes wasted, got %d", w)
	}
}

// TestPackedSlabsPartialWithHole covers a slab that an upload threshold sent
// off before it was full, and that then lost a piece: only the hole counts as
// dead space, while the part the slab was never filled with does not, and the
// level is measured against the whole slab, which is what is paid for either
// way.
func TestPackedSlabsPartialWithHole(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share, wg := newSlabTestFixture(t, db)

	// Uploaded with 600 of the 1000 bytes filled, then the middle 100 go.
	key := types.Hash256{1}
	plantPiece(t, db, share, acc, "a.txt", key, 0, 200)
	plantPiece(t, db, share, acc, "b.txt", key, 200, 100)
	plantPiece(t, db, share, acc, "c.txt", key, 300, 300)

	if _, err := db.DeleteFile(acc, share, "b.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	slabs, err := db.PackedSlabs(share, wg, slabSize, 0)
	if err != nil {
		t.Fatalf("PackedSlabs: %v", err)
	}
	assertPacked(t, slabs, []PackedSlab{
		{Key: key, Used: 500, Filled: 600, Pieces: 2},
	})

	// The hole is the 100 bytes b.txt held, not the 500 that separate what is
	// used from the slab size.
	if w := slabs[0].Wasted(); w != 100 {
		t.Fatalf("want the hole alone wasted, got %d bytes", w)
	}
	if f := slabs[0].Fragmentation(); f != 0.1 {
		t.Fatalf("want the hole measured against the slab size, got %v", f)
	}

	// So it is reported at a threshold of 10%, but not at the default 25%.
	stats, err := db.Fragmentation(share, wg, slabSize, 0.1)
	if err != nil {
		t.Fatalf("Fragmentation(0.1): %v", err)
	}
	if stats.Fragmented != 1 || stats.FragmentedWasted != 100 {
		t.Fatalf("want the slab reported at 10%%, got %+v", stats)
	}

	stats, err = db.Fragmentation(share, wg, slabSize, DefaultFragmentationThreshold)
	if err != nil {
		t.Fatalf("Fragmentation(default): %v", err)
	}
	if stats.Slabs != 1 || stats.Fragmented != 0 || stats.Wasted != 100 {
		t.Fatalf("want the slab counted but under the default threshold, got %+v", stats)
	}
}

// TestPackedSlabsUploadThreshold covers how the fragmentation threshold relates
// to the upload threshold that sent a slab off before it was full. A slab can
// never hold more dead space than it was filled with, so one uploaded at the
// minimum size is only ever reported by a fragmentation threshold below the
// share that minimum is of a slab.
func TestPackedSlabsUploadThreshold(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share, wg := newSlabTestFixture(t, db)

	// An upload threshold of 30% of a slab, uploaded at exactly that.
	const uploaded = slabSize * 3 / 10

	key := types.Hash256{1}
	plantPiece(t, db, share, acc, "a.txt", key, 0, 100)
	plantPiece(t, db, share, acc, "b.txt", key, 100, 100)
	plantPiece(t, db, share, acc, "c.txt", key, 200, uploaded-200)

	// All but the last piece go, which is as empty as the slab can get: one
	// piece has to survive, or the slab is an orphan rather than fragmented.
	for _, path := range []string{"a.txt", "b.txt"} {
		if _, err := db.DeleteFile(acc, share, path); err != nil {
			t.Fatalf("DeleteFile(%s): %v", path, err)
		}
	}

	slabs, err := db.PackedSlabs(share, wg, slabSize, 0)
	if err != nil {
		t.Fatalf("PackedSlabs: %v", err)
	}
	assertPacked(t, slabs, []PackedSlab{
		{Key: key, Used: uploaded - 200, Filled: uploaded, Pieces: 1},
	})

	// Below the upload threshold it is reported, at or above it never, however
	// much of it is dead: the whole slab only ever held 30% of a slab's worth.
	tests := []struct {
		threshold float64
		want      int
	}{
		{threshold: 0.1, want: 1},
		{threshold: 0.2, want: 1},
		{threshold: 0.25, want: 0},
		{threshold: 0.3, want: 0},
		{threshold: DefaultFragmentationThreshold, want: 0},
	}

	for _, tc := range tests {
		slabs, err := db.PackedSlabs(share, wg, slabSize, tc.threshold)
		if err != nil {
			t.Fatalf("PackedSlabs(%v): %v", tc.threshold, err)
		}
		if len(slabs) != tc.want {
			t.Fatalf("threshold %v: want %d slab(s), got %d", tc.threshold, tc.want, len(slabs))
		}

		stats, err := db.Fragmentation(share, wg, slabSize, tc.threshold)
		if err != nil {
			t.Fatalf("Fragmentation(%v): %v", tc.threshold, err)
		}
		if stats.Fragmented != tc.want || stats.Slabs != 1 {
			t.Fatalf("threshold %v: want %d of 1 slab(s) fragmented, got %+v", tc.threshold, tc.want, stats)
		}
	}
}

// TestPackedSlabsTailHole documents the blind spot of deriving the filled
// extent from the surviving pieces: a hole at the very end of a slab shrinks
// the extent with it and goes unreported. It under-reports, which is the safe
// way round, and a hole anywhere else is still seen.
func TestPackedSlabsTailHole(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share, wg := newSlabTestFixture(t, db)

	key := types.Hash256{1}
	plantPiece(t, db, share, acc, "a.txt", key, 0, 400)
	plantPiece(t, db, share, acc, "b.txt", key, 400, 300)
	plantPiece(t, db, share, acc, "c.txt", key, 700, 300)

	// The last piece goes: the extent shrinks to where b.txt ends.
	if _, err := db.DeleteFile(acc, share, "c.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	slabs, err := db.PackedSlabs(share, wg, slabSize, 0)
	if err != nil {
		t.Fatalf("PackedSlabs: %v", err)
	}
	if len(slabs) != 0 {
		t.Fatalf("want a hole at the tail to go unreported, got %+v", slabs)
	}

	// The one in the middle is still seen.
	if _, err := db.DeleteFile(acc, share, "a.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	slabs, err = db.PackedSlabs(share, wg, slabSize, 0)
	if err != nil {
		t.Fatalf("PackedSlabs: %v", err)
	}
	assertPacked(t, slabs, []PackedSlab{
		{Key: key, Used: 300, Filled: 700, Pieces: 1},
	})
}

// TestPackedSlabsThreshold verifies that only the slabs whose dead space
// reaches the threshold are listed, and that they come back most fragmented
// first.
func TestPackedSlabsThreshold(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share, wg := newSlabTestFixture(t, db)

	// Three slabs left 10%, 50% and 90% dead space by a deleted first piece.
	light, medium, heavy := types.Hash256{1}, types.Hash256{2}, types.Hash256{3}
	plantHole(t, db, share, acc, "light", light, 100)
	plantHole(t, db, share, acc, "medium", medium, 500)
	plantHole(t, db, share, acc, "heavy", heavy, 900)

	tests := []struct {
		threshold float64
		want      []types.Hash256
	}{
		{threshold: 0, want: []types.Hash256{heavy, medium, light}},
		{threshold: 0.1, want: []types.Hash256{heavy, medium, light}},
		{threshold: 0.25, want: []types.Hash256{heavy, medium}},
		{threshold: 0.5, want: []types.Hash256{heavy, medium}},
		{threshold: 0.75, want: []types.Hash256{heavy}},
		{threshold: 1, want: nil},
	}

	for _, tc := range tests {
		slabs, err := db.PackedSlabs(share, wg, slabSize, tc.threshold)
		if err != nil {
			t.Fatalf("PackedSlabs(%v): %v", tc.threshold, err)
		}
		if len(slabs) != len(tc.want) {
			t.Fatalf("threshold %v: want %d slab(s), got %d", tc.threshold, len(tc.want), len(slabs))
		}
		for i, key := range tc.want {
			if slabs[i].Key != key {
				t.Fatalf("threshold %v: want slab %d to be %v, got %v", tc.threshold, i, key, slabs[i].Key)
			}
		}
	}
}

// TestPackedSlabsWorkgroupScope verifies that a connection is only told about
// the slabs of its own workgroup, which are the ones its app account pins and
// pays for.
func TestPackedSlabsWorkgroupScope(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share, wg := newSlabTestFixture(t, db)
	bob, bobWG := newForeignAccount(t, db, "bob")

	// The slabs of the two workgroups are interleaved in the same share.
	mine, theirs := types.Hash256{1}, types.Hash256{2}
	plantHole(t, db, share, acc, "mine", mine, 300)
	plantHole(t, db, share, bob, "theirs", theirs, 500)

	slabs, err := db.PackedSlabs(share, wg, slabSize, 0)
	if err != nil {
		t.Fatalf("PackedSlabs: %v", err)
	}
	assertPacked(t, slabs, []PackedSlab{
		{Key: mine, Used: 700, Filled: 1000, Pieces: 1},
	})

	slabs, err = db.PackedSlabs(share, bobWG, slabSize, 0)
	if err != nil {
		t.Fatalf("PackedSlabs(bob): %v", err)
	}
	assertPacked(t, slabs, []PackedSlab{
		{Key: theirs, Used: 500, Filled: 1000, Pieces: 1},
	})
}

// TestFragmentationStats verifies that the summary counts every slab but only
// charges dead space to the ones that hold any, and that it matches what the
// listing reports, since the check and the API endpoint have to agree.
func TestFragmentationStats(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share, wg := newSlabTestFixture(t, db)

	stats, err := db.Fragmentation(share, wg, slabSize, 0.25)
	if err != nil {
		t.Fatalf("Fragmentation: %v", err)
	}
	if stats != (FragmentationStats{}) {
		t.Fatalf("want an empty share to have nothing packed, got %+v", stats)
	}

	// 10%, 50% and 90% dead space, plus two slabs nobody has touched: one
	// filled to the brim and one an aged upload left half empty.
	plantHole(t, db, share, acc, "light", types.Hash256{1}, 100)
	plantHole(t, db, share, acc, "medium", types.Hash256{2}, 500)
	plantHole(t, db, share, acc, "heavy", types.Hash256{3}, 900)
	plantPiece(t, db, share, acc, "big.txt", types.Hash256{4}, 0, slabSize)
	plantPiece(t, db, share, acc, "aged.txt", types.Hash256{5}, 0, 400)

	stats, err = db.Fragmentation(share, wg, slabSize, 0.25)
	if err != nil {
		t.Fatalf("Fragmentation: %v", err)
	}
	// The untouched slabs count towards the total but waste nothing, so that
	// the fragmented ones are reported against a denominator.
	want := FragmentationStats{
		Slabs:            5,
		Wasted:           100 + 500 + 900,
		Fragmented:       2,
		FragmentedWasted: 500 + 900,
	}
	if stats != want {
		t.Fatalf("want %+v, got %+v", want, stats)
	}

	// The two agree on what reaches the threshold.
	slabs, err := db.PackedSlabs(share, wg, slabSize, 0.25)
	if err != nil {
		t.Fatalf("PackedSlabs: %v", err)
	}
	var wasted uint64
	for _, slab := range slabs {
		wasted += slab.Wasted()
	}
	if len(slabs) != stats.Fragmented || wasted != stats.FragmentedWasted {
		t.Fatalf("want the listing to agree with %+v, got %d slab(s) wasting %d", stats, len(slabs), wasted)
	}
}

// TestFragmentationBadArgs verifies that the arguments a fragmentation level
// cannot be computed from are refused rather than reported as zero.
func TestFragmentationBadArgs(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	_, share, wg := newSlabTestFixture(t, db)

	tests := []struct {
		share     string
		slabSize  uint64
		threshold float64
	}{
		{share: "", slabSize: slabSize},
		{share: share, slabSize: 0},
		{share: share, slabSize: slabSize, threshold: -0.1},
		{share: share, slabSize: slabSize, threshold: 1.1},
	}

	for _, tc := range tests {
		if _, err := db.PackedSlabs(tc.share, wg, tc.slabSize, tc.threshold); err == nil {
			t.Fatalf("PackedSlabs(%q, %d, %v): want an error, got none", tc.share, tc.slabSize, tc.threshold)
		}
		if _, err := db.Fragmentation(tc.share, wg, tc.slabSize, tc.threshold); err == nil {
			t.Fatalf("Fragmentation(%q, %d, %v): want an error, got none", tc.share, tc.slabSize, tc.threshold)
		}
	}
}

// fillPieces hands each piece the bytes the slab holds at its slice, which is
// what the caller downloads before rebuffering them.
func fillPieces(t *testing.T, pieces []SlabPiece, slab []byte) []SlabPiece {
	t.Helper()

	for i := range pieces {
		end := pieces[i].DataOffset + pieces[i].DataLength
		if end > uint64(len(slab)) {
			t.Fatalf("piece %d reaches past the %d byte slab", i, len(slab))
		}
		pieces[i].Data = slab[pieces[i].DataOffset:end]
	}

	return pieces
}

// markTemporary hides the file again, which is the state of one whose upload
// has not been finalized.
func markTemporary(t *testing.T, db *Database, share, path string) {
	t.Helper()

	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE objects
			SET temporary = TRUE
			WHERE share_name = $1
				AND full_path = $2
		`, share, normalizePath(path))
		return err
	})
	if err != nil {
		t.Fatalf("mark %s temporary: %v", path, err)
	}
}

// TestRebufferSlab verifies that the live pieces of a fragmented slab go back
// into the upload queue holding their own data, that the files still read the
// same, and that the emptied slab is staged for unpinning.
func TestRebufferSlab(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share, wg := newSlabTestFixture(t, db)

	slab := make([]byte, slabSize)
	frand.Read(slab)

	key := types.Hash256{1}
	plantPiece(t, db, share, acc, "a.txt", key, 0, 400)
	plantPiece(t, db, share, acc, "b.txt", key, 400, 300)
	plantPiece(t, db, share, acc, "c.txt", key, 700, 300)

	// The hole b.txt leaves behind is what the rebuffering closes.
	if _, err := db.DeleteFile(acc, share, "b.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	pieces, err := db.SlabPieces(share, wg, key)
	if err != nil {
		t.Fatalf("SlabPieces: %v", err)
	}
	if len(pieces) != 2 {
		t.Fatalf("want the 2 surviving pieces, got %d", len(pieces))
	}

	staged, err := db.RebufferSlab(share, wg, key, fillPieces(t, pieces, slab))
	if err != nil {
		t.Fatalf("RebufferSlab: %v", err)
	}
	if len(staged) != 1 || staged[0] != key {
		t.Fatalf("want the emptied slab staged for unpinning, got %v", staged)
	}

	// Both files read back the same, now out of the database.
	if got := readPacked(t, db, acc, share, "a.txt", 400, nil); !bytes.Equal(got, slab[:400]) {
		t.Fatalf("a.txt reads back wrong after rebuffering")
	}
	if got := readPacked(t, db, acc, share, "c.txt", 300, nil); !bytes.Equal(got, slab[700:1000]) {
		t.Fatalf("c.txt reads back wrong after rebuffering")
	}

	// The queue holds them without an upload, so they age from now on.
	if n := pendingJobs(t, db); n != 2 {
		t.Fatalf("want both pieces queued, got %d jobs", n)
	}
	if n := queuedWithUpload(t, db); n != 0 {
		t.Fatalf("want no queued piece attached to an upload, got %d", n)
	}

	// Nothing references the slab any more, and the packer sees the pieces.
	slabs, err := db.PackedSlabs(share, wg, slabSize, 0)
	if err != nil {
		t.Fatalf("PackedSlabs: %v", err)
	}
	if len(slabs) != 0 {
		t.Fatalf("want the slab gone from the listing, got %+v", slabs)
	}

	// They fall short of a slab between them, so it takes the age they now
	// carry themselves to have them packed.
	if _, err := db.ClaimPackedSlab(share, wg, slabSize, 0, time.Hour); !errors.Is(err, ErrNoUploadJobs) {
		t.Fatalf("ClaimPackedSlab straight away: want %v, got %v", ErrNoUploadJobs, err)
	}
	backdateJobs(t, db, 2*time.Hour)

	jobs, err := db.ClaimPackedSlab(share, wg, slabSize, 0, time.Hour)
	if err != nil {
		t.Fatalf("ClaimPackedSlab: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("want both pieces claimable as one slab, got %d", len(jobs))
	}
	packed := append(append([]byte{}, jobs[0].Data...), jobs[1].Data...)
	if want := append(append([]byte{}, slab[:400]...), slab[700:1000]...); !bytes.Equal(packed, want) {
		t.Fatalf("the repacked slab does not hold what the pieces did")
	}
}

// TestSlabPiecesInFlight verifies that a slab a file that is still being
// written references is left alone: that upload may yet be abandoned.
func TestSlabPiecesInFlight(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share, wg := newSlabTestFixture(t, db)

	key := types.Hash256{1}
	plantPiece(t, db, share, acc, "a.txt", key, 0, 400)
	plantPiece(t, db, share, acc, "b.txt", key, 400, 600)
	markTemporary(t, db, share, "b.txt")

	if _, err := db.SlabPieces(share, wg, key); !errors.Is(err, ErrSlabInUse) {
		t.Fatalf("SlabPieces: want %v, got %v", ErrSlabInUse, err)
	}

	// The check is made again where it counts, so a write that starts after
	// the listing does not slip through.
	pieces := []SlabPiece{{MetadataID: 1, DataLength: 1, Data: []byte{0}}}
	if _, err := db.RebufferSlab(share, wg, key, pieces); !errors.Is(err, ErrSlabInUse) {
		t.Fatalf("RebufferSlab: want %v, got %v", ErrSlabInUse, err)
	}
}

// TestRebufferSlabChanged verifies that a slab whose pieces moved on since they
// were listed is left untouched, since the data downloaded for it is no longer
// what the files reference.
func TestRebufferSlabChanged(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share, wg := newSlabTestFixture(t, db)

	slab := make([]byte, slabSize)
	frand.Read(slab)

	key := types.Hash256{1}
	plantPiece(t, db, share, acc, "a.txt", key, 0, 400)
	plantPiece(t, db, share, acc, "b.txt", key, 400, 300)
	plantPiece(t, db, share, acc, "c.txt", key, 700, 300)

	if _, err := db.DeleteFile(acc, share, "b.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	pieces, err := db.SlabPieces(share, wg, key)
	if err != nil {
		t.Fatalf("SlabPieces: %v", err)
	}
	fillPieces(t, pieces, slab)

	// A.txt goes between the listing and the rewrite.
	if _, err := db.DeleteFile(acc, share, "a.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	if _, err := db.RebufferSlab(share, wg, key, pieces); !errors.Is(err, ErrSlabChanged) {
		t.Fatalf("RebufferSlab: want %v, got %v", ErrSlabChanged, err)
	}

	// What is left is still where it was, and nothing was queued.
	if n := pendingJobs(t, db); n != 0 {
		t.Fatalf("want an empty queue, got %d jobs", n)
	}
	if n := storedBuffers(t, db); n != 0 {
		t.Fatalf("want no buffers, got %d", n)
	}
	slabs, err := db.PackedSlabs(share, wg, slabSize, 0)
	if err != nil {
		t.Fatalf("PackedSlabs: %v", err)
	}
	assertPacked(t, slabs, []PackedSlab{
		{Key: key, Used: 300, Filled: 1000, Pieces: 1},
	})

	// A piece that appeared since the listing is one nothing was downloaded
	// for, and moving the rest would leave the slab pinned for it anyway.
	pieces, err = db.SlabPieces(share, wg, key)
	if err != nil {
		t.Fatalf("SlabPieces: %v", err)
	}
	fillPieces(t, pieces, slab)
	plantPiece(t, db, share, acc, "d.txt", key, 0, 400)

	if _, err := db.RebufferSlab(share, wg, key, pieces); !errors.Is(err, ErrSlabChanged) {
		t.Fatalf("RebufferSlab with a new piece: want %v, got %v", ErrSlabChanged, err)
	}
	if n := storedBuffers(t, db); n != 0 {
		t.Fatalf("want no buffers, got %d", n)
	}
}

// TestRebufferSlabSharedKey verifies that a slab whose key is referenced from
// outside this workgroup stays pinned. Slabs are content-addressed, so a file
// of the same content that another workgroup uploaded carries the same key,
// and unpinning the slab would take that file's data with it.
func TestRebufferSlabSharedKey(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share, wg := newSlabTestFixture(t, db)
	bob, _ := newForeignAccount(t, db, "bob")

	slab := make([]byte, slabSize)
	frand.Read(slab)

	// The packed slab of this workgroup, and the file of another workgroup
	// whose content came out identical to it.
	key := types.Hash256{1}
	plantPiece(t, db, share, acc, "mine.txt", key, 0, 400)
	plantPiece(t, db, share, acc, "gone.txt", key, 400, 300)
	plantPiece(t, db, share, acc, "kept.txt", key, 700, 300)
	plantPiece(t, db, share, bob, "theirs.txt", key, 0, slabSize)

	if _, err := db.DeleteFile(acc, share, "gone.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	pieces, err := db.SlabPieces(share, wg, key)
	if err != nil {
		t.Fatalf("SlabPieces: %v", err)
	}
	if len(pieces) != 2 {
		t.Fatalf("want only this workgroup's pieces, got %d", len(pieces))
	}

	staged, err := db.RebufferSlab(share, wg, key, fillPieces(t, pieces, slab))
	if err != nil {
		t.Fatalf("RebufferSlab: %v", err)
	}
	if len(staged) != 0 {
		t.Fatalf("want the shared slab left pinned, got %v", staged)
	}
	if got := readPacked(t, db, bob, share, "theirs.txt", slabSize, slab); !bytes.Equal(got, slab) {
		t.Fatalf("theirs.txt no longer reads out of the slab")
	}
}
