package stores

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"go.sia.tech/core/types"
)

// PackedSlab is an uploaded slab that is not filled to the brim, along with
// what is still referenced in it. The rest is paid for but holds nothing.
type PackedSlab struct {
	Key types.Hash256 `json:"key"`

	// Size is what the slab is paid for and Used is what is still referenced
	// in it, in bytes. Pieces counts the file slices that make up Used.
	Size   uint64 `json:"size"`
	Used   uint64 `json:"used"`
	Pieces int    `json:"pieces"`
}

// Wasted returns the dead space in the slab, in bytes.
func (s PackedSlab) Wasted() uint64 {
	if s.Used >= s.Size {
		return 0
	}
	return s.Size - s.Used
}

// Fragmentation returns the dead space as a fraction between 0 and 1.
func (s PackedSlab) Fragmentation() float64 {
	if s.Size == 0 {
		return 0
	}
	return float64(s.Wasted()) / float64(s.Size)
}

// FragmentationStats summarizes a share's slabs.
type FragmentationStats struct {
	// Slabs counts every slab the connection has pieces in, and Wasted is the
	// dead space across all of them, in bytes.
	Slabs  int    `json:"slabs"`
	Wasted uint64 `json:"wasted"`

	// Fragmented counts those of them that reach the threshold the stats were
	// taken with, and FragmentedWasted is the dead space in those alone.
	Fragmented       int    `json:"fragmented"`
	FragmentedWasted uint64 `json:"fragmentedWasted"`
}

// packedSlabsCTE gathers the slabs of the share ($1) and workgroup ($2), with
// what is still referenced in each. A slab belongs to one share and workgroup,
// the same scope the claims are made under, so this sees all of its pieces.
// Slabs that are still filled to the brim are kept, so that the counts have a
// denominator; they hold no dead space and are filtered out where that matters.
const packedSlabsCTE = `
	WITH slabs AS (
		SELECT
			m.slab_key,
			COUNT(*) AS pieces,
			SUM(m.data_length) AS used
		FROM metadata m
		JOIN objects o ON o.id = m.object_id
		WHERE m.slab_key IS NOT NULL
			AND o.share_name = $1
			AND o.workgroup = $2
		GROUP BY m.slab_key
	)
`

// fragmentedPredicate matches the slabs whose dead space reaches the threshold
// ($4, as the fraction that may still be used) of the slab size ($3). A slab
// that is filled to the brim never matches, not even at a threshold of zero.
const fragmentedPredicate = `used < $3::BIGINT AND used <= $3::BIGINT * $4::DOUBLE PRECISION`

// checkFragmentationArgs validates the common arguments and returns how much of
// a slab may still be used for it to count as fragmented.
func checkFragmentationArgs(share string, slabSize uint64, threshold float64) (float64, error) {
	if share == "" {
		return 0, errors.New("share name cannot be empty")
	}
	if slabSize == 0 {
		return 0, errors.New("slab size cannot be zero")
	}
	if threshold < 0 || threshold > 1 {
		return 0, fmt.Errorf("fragmentation threshold must be a fraction between 0 and 1, got %v", threshold)
	}
	return 1 - threshold, nil
}

// PackedSlabs lists the slabs of the given share and workgroup whose dead space
// reaches threshold, most fragmented first. A threshold of zero lists every
// slab that holds any dead space at all. slabSize is what a full slab holds; it
// follows from the connection's redundancy and is not recorded in the database.
func (db *Database) PackedSlabs(share string, workgroup int, slabSize uint64, threshold float64) (slabs []PackedSlab, err error) {
	maxUsed, err := checkFragmentationArgs(share, slabSize, threshold)
	if err != nil {
		return nil, err
	}

	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = packedSlabsCTE + `
			SELECT slab_key, pieces, used
			FROM slabs
			WHERE ` + fragmentedPredicate + `
			ORDER BY used, slab_key
		`

		rows, err := tx.Query(ctx, query, share, workgroup, int64(slabSize), maxUsed)
		if err != nil {
			return fmt.Errorf("failed to list packed slabs: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var (
				raw    []byte
				pieces int64
				used   int64
			)
			if err := rows.Scan(&raw, &pieces, &used); err != nil {
				return fmt.Errorf("failed to scan packed slab: %w", err)
			}
			if len(raw) != 32 {
				return fmt.Errorf("invalid slab key length: %d", len(raw))
			}

			slab := PackedSlab{
				Size:   slabSize,
				Used:   uint64(used),
				Pieces: int(pieces),
			}
			copy(slab.Key[:], raw)
			slabs = append(slabs, slab)
		}

		if err := rows.Err(); err != nil {
			return fmt.Errorf("failed to iterate packed slabs: %w", err)
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return
}

// Fragmentation summarizes the packed slabs of the given share and workgroup,
// counting those that reach threshold separately. It answers what PackedSlabs
// does without carrying a row per slab out of the database.
func (db *Database) Fragmentation(share string, workgroup int, slabSize uint64, threshold float64) (stats FragmentationStats, err error) {
	maxUsed, err := checkFragmentationArgs(share, slabSize, threshold)
	if err != nil {
		return FragmentationStats{}, err
	}

	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = packedSlabsCTE + `
			SELECT
				COUNT(*),
				COALESCE(SUM(GREATEST($3::BIGINT - used, 0)), 0),
				COUNT(*) FILTER (WHERE ` + fragmentedPredicate + `),
				COALESCE(SUM($3::BIGINT - used) FILTER (WHERE ` + fragmentedPredicate + `), 0)
			FROM slabs
		`

		var slabs, wasted, fragmented, fragmentedWasted int64
		err := tx.QueryRow(ctx, query, share, workgroup, int64(slabSize), maxUsed).Scan(
			&slabs,
			&wasted,
			&fragmented,
			&fragmentedWasted,
		)
		if err != nil {
			return fmt.Errorf("failed to summarize packed slabs: %w", err)
		}

		stats = FragmentationStats{
			Slabs:            int(slabs),
			Wasted:           uint64(wasted),
			Fragmented:       int(fragmented),
			FragmentedWasted: uint64(fragmentedWasted),
		}
		return nil
	})

	if err != nil {
		return FragmentationStats{}, err
	}
	return
}
