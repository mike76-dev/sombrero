package stores

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.sia.tech/core/types"
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

// assertPacked checks the slabs a listing reported against the expected key,
// used bytes and piece count, in order.
func assertPacked(t *testing.T, got []PackedSlab, want []PackedSlab) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("want %d packed slab(s), got %d: %+v", len(want), len(got), got)
	}
	for i := range want {
		if got[i].Key != want[i].Key || got[i].Used != want[i].Used || got[i].Pieces != want[i].Pieces {
			t.Fatalf("slab %d: want %+v, got %+v", i, want[i], got[i])
		}
		if got[i].Size != slabSize {
			t.Fatalf("slab %d: want size %d, got %d", i, slabSize, got[i].Size)
		}
	}
}

// TestPackedSlabs verifies which slabs count as packed and how much of each is
// reported as still in use: a slab that is filled to the brim is left out,
// whether it was uploaded whole or packed, and deleting a file punches a hole
// into the slab it was packed into.
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
	partial := types.Hash256{3}
	plantPiece(t, db, share, acc, "d.txt", partial, 0, 400)

	slabs, err := db.PackedSlabs(share, wg, slabSize, 0)
	if err != nil {
		t.Fatalf("PackedSlabs: %v", err)
	}
	assertPacked(t, slabs, []PackedSlab{
		{Key: partial, Used: 400, Pieces: 1},
	})

	// Deleting the middle file leaves a hole of its size behind.
	if _, err := db.DeleteFile(acc, share, "b.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	slabs, err = db.PackedSlabs(share, wg, slabSize, 0)
	if err != nil {
		t.Fatalf("PackedSlabs: %v", err)
	}
	assertPacked(t, slabs, []PackedSlab{
		{Key: partial, Used: 400, Pieces: 1},
		{Key: packed, Used: 700, Pieces: 2},
	})
	if f := slabs[1].Fragmentation(); f != 0.3 {
		t.Fatalf("want the emptied slab 30%% fragmented, got %v", f)
	}
	if w := slabs[1].Wasted(); w != 300 {
		t.Fatalf("want 300 bytes wasted, got %d", w)
	}
}

// TestPackedSlabsThreshold verifies that only the slabs whose dead space
// reaches the threshold are listed, and that they come back most fragmented
// first.
func TestPackedSlabsThreshold(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share, wg := newSlabTestFixture(t, db)

	// Three slabs that are 10%, 50% and 90% dead space.
	light, medium, heavy := types.Hash256{1}, types.Hash256{2}, types.Hash256{3}
	plantPiece(t, db, share, acc, "a.txt", light, 0, 900)
	plantPiece(t, db, share, acc, "b.txt", medium, 0, 500)
	plantPiece(t, db, share, acc, "c.txt", heavy, 0, 100)

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
	plantPiece(t, db, share, acc, "a.txt", mine, 0, 300)
	plantPiece(t, db, share, acc, "c.txt", mine, 300, 100)
	plantPiece(t, db, share, bob, "b.txt", theirs, 0, 700)

	slabs, err := db.PackedSlabs(share, wg, slabSize, 0)
	if err != nil {
		t.Fatalf("PackedSlabs: %v", err)
	}
	assertPacked(t, slabs, []PackedSlab{
		{Key: mine, Used: 400, Pieces: 2},
	})

	slabs, err = db.PackedSlabs(share, bobWG, slabSize, 0)
	if err != nil {
		t.Fatalf("PackedSlabs(bob): %v", err)
	}
	assertPacked(t, slabs, []PackedSlab{
		{Key: theirs, Used: 700, Pieces: 1},
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

	// 10%, 50% and 90% dead space, plus a slab that is filled to the brim.
	plantPiece(t, db, share, acc, "a.txt", types.Hash256{1}, 0, 900)
	plantPiece(t, db, share, acc, "b.txt", types.Hash256{2}, 0, 500)
	plantPiece(t, db, share, acc, "c.txt", types.Hash256{3}, 0, 100)
	plantPiece(t, db, share, acc, "big.txt", types.Hash256{4}, 0, slabSize)

	stats, err = db.Fragmentation(share, wg, slabSize, 0.25)
	if err != nil {
		t.Fatalf("Fragmentation: %v", err)
	}
	// The full slab counts towards the total but wastes nothing, so that the
	// fragmented ones are reported against a denominator.
	want := FragmentationStats{
		Slabs:            4,
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
