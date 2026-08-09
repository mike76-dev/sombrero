package stores

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
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
		if err := db.shares.AddConnection(wg, share, appKey); err != nil {
			return fmt.Errorf("failed to connect share: %w", err)
		}
		return nil
	})
}

// Connection is a workgroup's connection to a share: the workgroup it belongs
// to, and the app key that authenticates it with the storage backend.
type Connection struct {
	Workgroup uuid.UUID
	AppKey    types.PrivateKey
}

// ShareConnections returns the connections of the given share. A connection
// outlives the client that serves it — the app key is what it is made of — so
// this is what a share is connected to, whether or not anything is running for
// it right now.
func (db *Database) ShareConnections(share string) (conns []Connection, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT w.uuid, c.app_key
			FROM connections c
			JOIN workgroups w ON w.id = c.workgroup
			WHERE c.share_name = $1
		`

		rows, err := tx.Query(ctx, query, share)
		if err != nil {
			return fmt.Errorf("failed to retrieve connections: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var conn Connection
			if err := rows.Scan(&conn.Workgroup, &conn.AppKey); err != nil {
				return fmt.Errorf("failed to scan connection: %w", err)
			}
			conns = append(conns, conn)
		}

		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return
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
		if err := db.shares.RemoveConnection(wg, share); err != nil {
			return fmt.Errorf("failed to disconnect share: %w", err)
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
		} else if err != nil {
			return fmt.Errorf("failed to retrieve connection: %w", err)
		}
		connected = true
		return nil
	})
	if err != nil {
		return false, nil, err
	}
	return connected, appKey, nil
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
