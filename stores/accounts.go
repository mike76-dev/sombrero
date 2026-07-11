package stores

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/mike76-dev/sombrero/utils"
	"golang.org/x/crypto/md4"
)

// Account represents a user account that can connect to particular shares.
type Account struct {
	ID        int    `json:"id"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	NTHash    []byte `json:"-"`
	Workgroup string `json:"workgroup"`
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
			return nil
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
			return nil
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
	u, err := uuid.Parse(acc.Workgroup)
	if err != nil {
		return fmt.Errorf("invalid workgroup UUID: %w", err)
	}
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			INSERT INTO accounts (account_name, password_hash, workgroup)
			VALUES ($1, $2, (SELECT id FROM workgroups WHERE uuid = $3))
		`

		h := md4.New()
		h.Write(utils.EncodeStringToBytes(acc.Password))
		acc.NTHash = h.Sum(nil)

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

// RemoveAccount removes the specified account from the database.
func (db *Database) RemoveAccount(username, workgroup string) error {
	u, err := uuid.Parse(workgroup)
	if err != nil {
		return fmt.Errorf("invalid workgroup UUID: %w", err)
	}
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			DELETE FROM accounts
			WHERE account_name = $1
			AND workgroup = (SELECT id FROM workgroups WHERE uuid = $2)
		`
		_, err := tx.Exec(ctx, query, username, u[:])
		if err != nil {
			return fmt.Errorf("failed to remove account: %w", err)
		}
		db.shares.RemoveAccess(Account{Username: username, Workgroup: workgroup})
		return nil
	})
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
		const query = `
			DELETE FROM accounts
			WHERE workgroup = (SELECT id FROM workgroups WHERE uuid = $1)
		`
		_, err := tx.Exec(ctx, query, u[:])
		if err != nil {
			return fmt.Errorf("failed to remove accounts: %w", err)
		}
		for _, acc := range accs {
			db.shares.RemoveAccess(acc)
		}
		return nil
	})
}
