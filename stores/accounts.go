package stores

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/mike76-dev/sombrero/utils"
	"golang.org/x/crypto/md4"
)

// ErrAccountNotFound is returned by the account lookups when there is no such account. It has
// to be an error rather than a zero Account, because a zero Account is a usable-looking value
// that the callers cannot tell apart from a real one: its NTHash is nil, and authenticating
// against a nil hash succeeds for anybody who bothers to compute NTOWFv2 over it.
var ErrAccountNotFound = errors.New("account not found")

// ErrAccountExists is returned when an account is added under a name that
// another account of the same workgroup already has.
var ErrAccountExists = errors.New("an account with this name already exists in this workgroup")

// Account represents a user account that can connect to particular shares.
type Account struct {
	ID        int    `json:"id"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	NTHash    []byte `json:"-"`
	Workgroup string `json:"workgroup"`
}

// emptyNTHash is what an account with no password hashes to.
var emptyNTHash = ntHash("")

// ntHash returns the NT hash of the password, which is what an account is
// stored and authenticated with.
func ntHash(password string) []byte {
	h := md4.New()
	h.Write(utils.EncodeStringToBytes(password))
	return h.Sum(nil)
}

// Passwordless reports whether the account is one that anybody can log in as,
// which is what makes it a guest account. Such an account still authenticates
// like any other: the client has to compute its response over the same empty
// password, which is what keeps the session keys of both sides in step.
func (acc Account) Passwordless() bool {
	return bytes.Equal(acc.NTHash, emptyNTHash)
}

// GetAccountByID tries to retrieve the account by its ID.
func (db *Database) GetAccountByID(id int) (acc Account, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT a.account_name, a.password_hash, w.uuid
			FROM accounts a
			JOIN workgroups w ON w.id = a.workgroup
			WHERE a.id = $1
		`
		var username string
		var pwh []byte
		var u uuid.UUID
		err = tx.QueryRow(ctx, query, id).Scan(&username, &pwh, &u)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAccountNotFound
		} else if err != nil {
			return fmt.Errorf("failed to retrieve account: %w", err)
		}
		acc = Account{id, username, "", pwh, u.String()}
		return nil
	})
	return
}

// FindAccount tries to retrieve the account by the username and the workgroup UUID.
func (db *Database) FindAccount(username, workgroup string) (acc Account, err error) {
	u, err := uuid.Parse(workgroup)
	if err != nil {
		return acc, fmt.Errorf("invalid workgroup UUID: %w", err)
	}
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT a.id, a.password_hash
			FROM accounts a
			JOIN workgroups w ON w.id = a.workgroup
			WHERE a.account_name = $1
			AND w.uuid = $2
		`
		var id int
		var pwh []byte
		err = tx.QueryRow(ctx, query, username, u[:]).Scan(&id, &pwh)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAccountNotFound
		} else if err != nil {
			return fmt.Errorf("failed to retrieve account: %w", err)
		}
		acc = Account{id, username, "", pwh, workgroup}
		return nil
	})
	return
}

// AddAccount adds a new account to the database.
func (db *Database) AddAccount(acc Account) error {
	if IsAnonymousWorkgroup(acc.Workgroup) {
		return ErrReservedWorkgroup
	}
	u, err := uuid.Parse(acc.Workgroup)
	if err != nil {
		return fmt.Errorf("invalid workgroup UUID: %w", err)
	}
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		// The unique constraint would refuse this anyway; asking first is what
		// tells a taken name from a database that is having trouble.
		const taken = `
			SELECT 1
			FROM accounts a
			JOIN workgroups w ON w.id = a.workgroup
			WHERE a.account_name = $1 AND w.uuid = $2
		`
		var exists int
		if err := tx.QueryRow(ctx, taken, acc.Username, u[:]).Scan(&exists); err == nil {
			return ErrAccountExists
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("failed to look up the account name: %w", err)
		}

		const query = `
			INSERT INTO accounts (account_name, password_hash, workgroup)
			VALUES ($1, $2, (SELECT id FROM workgroups WHERE uuid = $3))
		`

		acc.NTHash = ntHash(acc.Password)

		_, err := tx.Exec(ctx, query, acc.Username, acc.NTHash, u[:])
		if err != nil {
			return fmt.Errorf("failed to add account: %w", err)
		} else {
			return nil
		}
	})
}

// HasAccount returns true if there is such account in the database.
func (db *Database) HasAccount(username, workgroup string) (bool, error) {
	u, err := uuid.Parse(workgroup)
	if err != nil {
		return false, fmt.Errorf("invalid workgroup UUID: %w", err)
	}
	var count int
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT COUNT(*)
			FROM accounts a
			JOIN workgroups w ON w.id = a.workgroup
			WHERE a.account_name = $1
			AND w.uuid = $2
		`
		return tx.QueryRow(ctx, query, username, u[:]).Scan(&count)
	})
	return count > 0, err
}

// RemoveAccount removes the specified account from the database, and with it the
// folders and files it owned. What those files were made of is released the way
// deleting them one by one would: see accountStorage.
func (db *Database) RemoveAccount(username, workgroup string) error {
	u, err := uuid.Parse(workgroup)
	if err != nil {
		return fmt.Errorf("invalid workgroup UUID: %w", err)
	}
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const owned = `
			SELECT id FROM accounts
			WHERE account_name = $1
			AND workgroup = (SELECT id FROM workgroups WHERE uuid = $2)
		`
		storage, err := collectAccountStorage(ctx, tx, owned, username, u[:])
		if err != nil {
			return err
		}

		const query = `
			DELETE FROM accounts
			WHERE account_name = $1
			AND workgroup = (SELECT id FROM workgroups WHERE uuid = $2)
		`
		if _, err := tx.Exec(ctx, query, username, u[:]); err != nil {
			return fmt.Errorf("failed to remove account: %w", err)
		}
		if err := storage.release(ctx, tx); err != nil {
			return err
		}
		db.shares.RemoveAccess(Account{Username: username, Workgroup: workgroup})
		return nil
	})
}

// accountStorage is what the files of some accounts are made of: the buffers
// that hold what has not been uploaded, and the slabs that hold the rest, by the
// share and workgroup that pinned them.
//
// Deleting an account takes its files with it, by cascade, and a cascade knows
// nothing of slabs: left at that, the slabs only those files referenced would
// stay pinned and paid for with nothing pointing at them. So what the files are
// made of is noted before the delete and released after it.
type accountStorage struct {
	buffers []uint64
	slabs   map[pinner][][]byte
}

// pinner is a share and the workgroup whose connection to it pinned a slab.
type pinner struct {
	share     string
	workgroup int
}

// collectAccountStorage notes what the files of the accounts the query selects
// are made of. It has to run before the accounts are deleted.
func collectAccountStorage(ctx context.Context, tx pgx.Tx, accounts string, args ...any) (accountStorage, error) {
	storage := accountStorage{slabs: make(map[pinner][][]byte)}

	rows, err := tx.Query(ctx, `
		SELECT DISTINCT o.share_name, o.workgroup, m.buffer_id, m.slab_key
		FROM metadata m
		JOIN objects o ON o.id = m.object_id
		WHERE o.account IN (`+accounts+`)
	`, args...)
	if err != nil {
		return storage, fmt.Errorf("failed to collect what the account's files are made of: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			p   pinner
			bid *uint64
			key []byte
		)
		if err := rows.Scan(&p.share, &p.workgroup, &bid, &key); err != nil {
			return storage, fmt.Errorf("failed to scan a storage reference: %w", err)
		}
		if bid != nil {
			storage.buffers = append(storage.buffers, *bid)
		}
		if key != nil {
			storage.slabs[p] = append(storage.slabs[p], key)
		}
	}

	return storage, rows.Err()
}

// release drops the buffers nothing refers to any more and stages the slabs
// nothing refers to for unpinning. It has to run after the accounts are deleted.
func (s accountStorage) release(ctx context.Context, tx pgx.Tx) error {
	for _, bid := range s.buffers {
		if _, err := tx.Exec(ctx, `
			DELETE FROM buffers b
			WHERE b.id = $1
				AND NOT EXISTS (SELECT 1 FROM metadata m WHERE m.buffer_id = b.id)
		`, bid); err != nil {
			return fmt.Errorf("failed to delete an orphaned buffer: %w", err)
		}
	}
	for p, keys := range s.slabs {
		if _, err := unreferencedSlabs(ctx, tx, p.share, p.workgroup, keys); err != nil {
			return err
		}
	}

	return nil
}

// FindAccounts returns all accounts of the specified workgroup.
func (db *Database) FindAccounts(workgroup string) (accs []Account, err error) {
	u, err := uuid.Parse(workgroup)
	if err != nil {
		return nil, fmt.Errorf("invalid workgroup UUID: %w", err)
	}
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT a.id, a.account_name, a.password_hash
			FROM accounts a
			JOIN workgroups w ON w.id = a.workgroup
			WHERE w.uuid = $1
		`
		rows, err := tx.Query(ctx, query, u[:])
		if err != nil {
			return fmt.Errorf("failed to fetch accounts: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int
			var username string
			var pwh []byte
			if err := rows.Scan(&id, &username, &pwh); err != nil {
				return fmt.Errorf("failed to fetch accounts: %w", err)
			}
			accs = append(accs, Account{
				ID:        id,
				Username:  username,
				Password:  "",
				NTHash:    pwh,
				Workgroup: workgroup,
			})
		}
		return nil
	})
	return
}

// RemoveAccounts removes all accounts of the specified workgroup.
func (db *Database) RemoveAccounts(workgroup string) error {
	u, err := uuid.Parse(workgroup)
	if err != nil {
		return fmt.Errorf("invalid workgroup UUID: %w", err)
	}
	accs, err := db.FindAccounts(workgroup)
	if err != nil {
		return err
	}
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const owned = `
			SELECT id FROM accounts
			WHERE workgroup = (SELECT id FROM workgroups WHERE uuid = $1)
		`
		storage, err := collectAccountStorage(ctx, tx, owned, u[:])
		if err != nil {
			return err
		}

		const query = `
			DELETE FROM accounts
			WHERE workgroup = (SELECT id FROM workgroups WHERE uuid = $1)
		`
		if _, err := tx.Exec(ctx, query, u[:]); err != nil {
			return fmt.Errorf("failed to remove accounts: %w", err)
		}
		if err := storage.release(ctx, tx); err != nil {
			return err
		}
		for _, acc := range accs {
			db.shares.RemoveAccess(acc)
		}
		return nil
	})
}
