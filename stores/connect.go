package stores

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"go.sia.tech/core/types"
)

// AddConnection creates a connection between a workgroup and a share.
func (db *Database) AddConnection(wg Workgroup, share Share, appKey types.PrivateKey) error {
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			INSERT INTO connections (workgroup, share_name, app_key)
			VALUES ($1, $2, $3)
			ON CONFLICT (workgroup, share_name) DO NOTHING
		`
		_, err := tx.Exec(ctx, query, wg.ID, share.Name, appKey)
		if err != nil {
			return fmt.Errorf("failed to add connection: %w", err)
		}
		return nil
	})
}

// RemoveConnection removes the connection between a workgroup and a share.
func (db *Database) RemoveConnection(wg Workgroup, share Share) error {
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			DELETE FROM connections
			WHERE workgroup = $1
			AND share_name = $2
		`
		_, err := tx.Exec(ctx, query, wg.ID, share.Name)
		if err != nil {
			return fmt.Errorf("failed to remove connection: %w", err)
		}
		return nil
	})
}

// IsConnected checks if a connection exists between a workgroup and a share.
func (db *Database) IsConnected(wg Workgroup, share Share) (bool, types.PrivateKey, error) {
	var appKey types.PrivateKey
	var connected bool
	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT app_key
			FROM connections
			WHERE workgroup = $1
			AND share_name = $2
		`
		err := tx.QueryRow(ctx, query, wg.ID, share.Name).Scan(&appKey)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		connected = true
		return nil
	})
	return connected, appKey, err
}

// SetAppKey sets the app key for the connection between a workgroup and a share.
func (db *Database) SetAppKey(wg Workgroup, share Share, key types.PrivateKey) error {
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			UPDATE connections
			SET app_key = $3
			WHERE workgroup = $1
			AND share_name = $2
		`
		_, err := tx.Exec(ctx, query, wg.ID, share.Name, key)
		if err != nil {
			return fmt.Errorf("failed to set connection key: %w", err)
		}
		return nil
	})
}
