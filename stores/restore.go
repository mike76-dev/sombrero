package stores

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/core/types"
)

var (
	// ErrNotACatalog is returned for a description that says nothing about what
	// it belongs to, which a restore cannot place.
	ErrNotACatalog = errors.New("the description is not a catalog: it says nothing about what it belongs to")

	// ErrConnectionExists is returned when a catalog is restored over a
	// connection the server has already, without being told to.
	ErrConnectionExists = errors.New("the workgroup is connected to the share already")

	// ErrShareMismatch is returned when a share of the catalog's name is
	// registered already, but serves something else.
	ErrShareMismatch = errors.New("a share of that name is registered already, serving something else")
)

// RestoreOptions says how a catalog is applied.
type RestoreOptions struct {
	// Force applies a catalog to a connection the server has already, adding
	// what is missing and leaving what is there as it is. Without it such a
	// restore is refused, so that one cannot be run over a live share by
	// mistake.
	Force bool

	// Held reports whether the account still holds the object. A catalog can
	// only point at data, and a file whose data was unpinned since the catalog
	// was written is left out rather than made as an entry nothing can read.
	// Nil trusts the catalog.
	Held func(types.Hash256) bool
}

// maxMissingPaths is how many of the files left out a restore names.
const maxMissingPaths = 20

// RestoreStats is what a restore came to, on top of what applying the folders
// and files did: the accounts and policies that were made, and the files left
// out because their data is no longer in the account, the first few by name.
type RestoreStats struct {
	ApplyStats
	Share        string
	Workgroup    uuid.UUID
	Accounts     int
	Policies     int
	Missing      int
	MissingPaths []string
}

// Restore recreates a connection from its catalog: the share, the workgroup with
// its accounts and policies, the connection with its app key, and then every
// folder and file, each to the account that owned it. What is there already is
// left as it is, so a restore that was cut short can be run again.
func (db *Database) Restore(ctx context.Context, r *transfer.Reader, opts RestoreOptions) (RestoreStats, error) {
	conn := r.Connection()
	if conn == nil {
		return RestoreStats{}, ErrNotACatalog
	}
	stats := RestoreStats{Share: conn.Share.Name, Workgroup: conn.Workgroup.UUID}

	share, err := db.restoreShare(conn.Share)
	if err != nil {
		return stats, err
	}
	wg, err := db.restoreWorkgroup(conn.Workgroup)
	if err != nil {
		return stats, err
	}

	connected, _, err := db.IsConnected(wg, share)
	if err != nil {
		return stats, err
	}
	if connected && !opts.Force {
		return stats, ErrConnectionExists
	}
	if !connected {
		if !opts.Force {
			populated, err := db.holdsRows(share.Name, wg.ID)
			if err != nil {
				return stats, err
			}
			if populated {
				return stats, fmt.Errorf("%w: the share holds files of the workgroup", ErrConnectionExists)
			}
		}
		var appKey types.PrivateKey
		if len(conn.AppKey) > 0 {
			appKey = types.PrivateKey(conn.AppKey)
		}
		if err := db.AddConnection(wg, share, appKey); err != nil {
			return stats, err
		}
	}

	for _, acc := range conn.Accounts {
		made, err := db.restoreAccount(wg, acc)
		if err != nil {
			return stats, err
		}
		if made {
			stats.Accounts++
		}
	}
	accounts, err := db.FindAccounts(wg.UUID.String())
	if err != nil {
		return stats, err
	}
	owners := make(map[string]Account, len(accounts))
	for _, acc := range accounts {
		owners[acc.Username] = acc
	}

	for _, p := range conn.Policies {
		acc, ok := owners[p.Account]
		if !ok {
			return stats, fmt.Errorf("the policy for %q names an account the catalog does not have", p.Account)
		}
		if err := db.SetAccessRights(AccessRights{
			ShareName: share.Name, AccountID: acc.ID,
			ReadAccess: p.Read, WriteAccess: p.Write, DeleteAccess: p.Delete, ExecuteAccess: p.Execute,
		}); err != nil {
			return stats, err
		}
		stats.Policies++
	}

	// The folders and files, each to its owner.
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		dir, file, err := r.Next()
		if errors.Is(err, io.EOF) {
			return stats, nil
		}
		if err != nil {
			return stats, err
		}

		var owner, path string
		if dir != nil {
			owner, path = dir.Owner, dir.Path
		} else {
			owner, path = file.Owner, file.Path
		}
		acc, ok := owners[owner]
		if !ok {
			return stats, fmt.Errorf("%q belongs to %q, an account the catalog does not have", path, owner)
		}
		if file != nil && opts.Held != nil && !held(*file, opts.Held) {
			stats.Missing++
			if len(stats.MissingPaths) < maxMissingPaths {
				stats.MissingPaths = append(stats.MissingPaths, file.Path)
			}
			continue
		}

		target := TransferTarget{Share: share.Name, Workgroup: wg.ID, Owner: acc}
		if err := db.applyRecord(target, dir, file, &stats.ApplyStats); err != nil {
			return stats, err
		}
	}
}

// ServerRestoreStats is what restoring a catalog of the server came to: what was
// made, with what was there already left as it was.
type ServerRestoreStats struct {
	Shares     int
	Workgroups int
	Accounts   int
	Bans       int
}

// RestoreServer recreates what a catalog of the server describes: the shares,
// the workgroups with their accounts, and the bans. Nothing that is there
// already is touched, and no connection is made.
func (db *Database) RestoreServer(ctx context.Context, r *transfer.Reader) (ServerRestoreStats, error) {
	var stats ServerRestoreStats
	server := r.Server()
	if server == nil {
		return stats, ErrNotACatalog
	}

	for _, s := range server.Shares {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		existing, err := db.GetShare(s.Name)
		if err != nil {
			return stats, err
		}
		if existing.Name != "" {
			continue
		}
		if _, err := db.restoreShare(s); err != nil {
			return stats, err
		}
		stats.Shares++
	}

	for _, wa := range server.Workgroups {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		existing, err := db.FindWorkgroup(uuid.UUID(wa.Workgroup.UUID))
		if err != nil {
			return stats, err
		}
		wg := existing
		if existing.ID == 0 {
			if wg, err = db.restoreWorkgroup(wa.Workgroup); err != nil {
				return stats, err
			}
			stats.Workgroups++
		}
		for _, acc := range wa.Accounts {
			made, err := db.restoreAccount(wg, acc)
			if err != nil {
				return stats, err
			}
			if made {
				stats.Accounts++
			}
		}
	}

	for _, ban := range server.Bans {
		banned, _, err := db.IsBanned(ban.Host)
		if err != nil {
			return stats, err
		}
		if banned {
			continue
		}
		if err := db.BanHost(ban.Host, ban.Reason); err != nil {
			return stats, err
		}
		stats.Bans++
	}

	return stats, nil
}

// held reports whether every object a file points at is still in the account.
// Bytes the catalog carries itself need no object.
func held(file transfer.File, check func(types.Hash256) bool) bool {
	for _, part := range file.Parts {
		if part.Object != (types.Hash256{}) && !check(part.Object) {
			return false
		}
	}

	return true
}

// restoreShare registers the share the catalog describes, or returns the one of
// that name that is registered already, provided it serves the same thing.
func (db *Database) restoreShare(s transfer.Share) (Share, error) {
	existing, err := db.GetShare(s.Name)
	if err != nil {
		return Share{}, err
	}
	if existing.Name != "" {
		if existing.Type != s.Type || existing.ServerName != s.Server {
			return Share{}, fmt.Errorf("%w: %s at %s", ErrShareMismatch, existing.Type, existing.ServerName)
		}
		return existing, nil
	}

	err = db.RegisterShare(Share{
		Name: s.Name, Type: s.Type, ServerName: s.Server, Password: s.Password, Bucket: s.Bucket, Remark: s.Remark,
		DataShards: s.DataShards, ParityShards: s.ParityShards,
		AllowGuest: s.AllowGuest, AllowAnonymous: s.AllowAnonymous, PublicDir: s.PublicDir, SkipBackup: s.SkipBackup,
	})
	if err != nil {
		return Share{}, err
	}

	return db.GetShare(s.Name)
}

// restoreWorkgroup adds the workgroup the catalog describes, or returns the one
// of that identity that is there already, as it is.
func (db *Database) restoreWorkgroup(w transfer.Workgroup) (Workgroup, error) {
	u := uuid.UUID(w.UUID)
	existing, err := db.FindWorkgroup(u)
	if err != nil {
		return Workgroup{}, err
	}
	if existing.ID != 0 {
		return existing, nil
	}

	dirs := make([]PublicDir, 0, len(w.PublicDirs))
	for _, pd := range w.PublicDirs {
		dirs = append(dirs, PublicDir{Path: pd.Path, ReadOnly: pd.ReadOnly, CaseSensitive: pd.CaseSensitive})
	}
	if err := db.AddWorkgroup(Workgroup{UUID: u, Name: w.Name, PublicDirs: dirs}); err != nil {
		return Workgroup{}, fmt.Errorf("failed to add the workgroup %s: %w", u, err)
	}

	return db.FindWorkgroup(u)
}

// restoreAccount adds an account with the hash it had, and reports whether it
// was added: one of that name is left as it is.
func (db *Database) restoreAccount(wg Workgroup, acc transfer.Account) (bool, error) {
	var made bool
	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			INSERT INTO accounts (account_name, password_hash, workgroup, created_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (account_name, workgroup) DO NOTHING
		`
		tag, err := tx.Exec(ctx, query, acc.Name, acc.PasswordHash, wg.ID, at(acc.CreatedAt))
		if err != nil {
			return fmt.Errorf("failed to add the account %q: %w", acc.Name, err)
		}
		made = tag.RowsAffected() > 0

		return nil
	})

	return made, err
}

// holdsRows reports whether the share holds any folder or file of the workgroup,
// which is what a restore must not run over unasked.
func (db *Database) holdsRows(share string, workgroup int) (bool, error) {
	var populated bool
	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT EXISTS (SELECT 1 FROM objects WHERE share_name = $1 AND workgroup = $2)
				OR EXISTS (SELECT 1 FROM directories WHERE share_name = $1 AND workgroup = $2)
		`
		return tx.QueryRow(ctx, query, share, workgroup).Scan(&populated)
	})
	if err != nil {
		return false, fmt.Errorf("failed to look at what the share holds: %w", err)
	}

	return populated, nil
}
