package stores

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AccessRights describes the access policies of a user account.
type AccessRights struct {
	ShareName     string
	AccountID     int
	ReadAccess    bool
	WriteAccess   bool
	DeleteAccess  bool
	ExecuteAccess bool
}

// GetAccessRights retrieves the access policy for the given account.
func (db *Database) GetAccessRights(share Share, acc Account) (ar AccessRights, err error) {
	if share.Name == "" {
		return AccessRights{}, nil
	}
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT read_access, write_access, delete_access, execute_access
			FROM policies
			WHERE share_name = $1
			AND account = $2
		`
		var ra, wa, da, ea bool
		err = tx.QueryRow(ctx, query, share.Name, acc.ID).Scan(&ra, &wa, &da, &ea)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("failed to retrieve policy: %w", err)
		}
		ar = AccessRights{
			ShareName:     share.Name,
			AccountID:     acc.ID,
			ReadAccess:    ra,
			WriteAccess:   wa,
			DeleteAccess:  da,
			ExecuteAccess: ea,
		}
		return nil
	})
	return
}

// SetAccessRights stores the access policy in the database.
func (db *Database) SetAccessRights(ar AccessRights) error {
	sh, err := db.GetShare(ar.ShareName)
	if err != nil {
		return fmt.Errorf("failed to retrieve share: %w", err)
	}
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			INSERT INTO policies (share_name, account, read_access, write_access, delete_access, execute_access)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (share_name, account) DO UPDATE
			SET read_access = EXCLUDED.read_access,
				write_access = EXCLUDED.write_access,
				delete_access = EXCLUDED.delete_access,
				execute_access = EXCLUDED.execute_access
		`
		_, err := tx.Exec(ctx, query, ar.ShareName, ar.AccountID, ar.ReadAccess, ar.WriteAccess, ar.DeleteAccess, ar.ExecuteAccess)
		if err != nil {
			return fmt.Errorf("failed to update policy: %w", err)
		}
		if err := db.shares.UpdateAccessRights(sh, ar); err != nil {
			return fmt.Errorf("failed to update access rights: %w", err)
		}
		return nil
	})
}

// RemoveAccessRights removes the access policy to the share for the given account.
func (db *Database) RemoveAccessRights(share Share, acc Account) error {
	if share.Name == "" {
		return nil
	}
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			DELETE FROM policies
			WHERE share_name = $1
			AND account = $2
		`
		_, err := tx.Exec(ctx, query, share.Name, acc.ID)
		if err != nil {
			return fmt.Errorf("failed to remove policy: %w", err)
		} else if err := db.shares.UpdateAccessRights(share, AccessRights{AccountID: acc.ID}); err != nil {
			return fmt.Errorf("failed to update access rights: %w", err)
		}
		return nil
	})
}

// ClearAccessRights removes all access rights for the given account.
func (db *Database) ClearAccessRights(acc Account) error {
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			DELETE FROM policies
			WHERE account = $1
		`
		_, err := tx.Exec(ctx, query, acc.ID)
		if err != nil {
			return fmt.Errorf("failed to remove policies: %w", err)
		} else {
			db.shares.RemoveAccess(acc)
			return nil
		}
	})
}

// FlagsFromAccessRights converts an AccessRights structure into SMB2 flags.
func FlagsFromAccessRights(ar AccessRights) uint32 {
	var flags uint32
	if ar.ReadAccess {
		flags |= 0x80120089 // FILE_READ_DATA | FILE_READ_EA | FILE_READ_ATTRIBUTES | READ_CONTROL | SYNCHRONIZE | GENERIC_READ
	}

	if ar.WriteAccess {
		flags |= 0x400c0116 // FILE_WRITE_DATA | FILE_APPEND_DATA | FILE_WRITE_EA | FILE_WRITE_ATTRIBUTES | WRITE_DAC | WRITE_OWNER | GENERIC_WRITE
	}

	if ar.DeleteAccess {
		flags |= 0x00010040 // FILE_DELETE_CHILD | DELETE
	}

	if ar.ExecuteAccess {
		flags |= 0x20000020 // FILE_EXECUTE | GENERIC_EXECUTE
	}

	if ar.ReadAccess && ar.WriteAccess && ar.DeleteAccess && ar.ExecuteAccess {
		flags |= 0x12000000 // GENERIC_ALL | MAXIMUM_ALLOWED
	}

	return flags
}
