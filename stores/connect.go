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

// HasConnections reports whether any workgroup is connected to the share, for
// the callers that only need to know that much and have no business with the
// app keys that ShareConnections returns. Only the database-backed store has
// it: a Lite server serves renterd shares alone, which nothing asks this of.
func (db *Database) HasConnections(share string) (bool, error) {
	var exists bool
	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `SELECT EXISTS (SELECT 1 FROM connections WHERE share_name = $1)`
		return tx.QueryRow(ctx, query, share).Scan(&exists)
	})
	if err != nil {
		return false, fmt.Errorf("failed to check the connections of the share: %w", err)
	}

	return exists, nil
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

// AppKeyForServer returns the app key of a connection the workgroup already has
// to an indexd share on the given server, or nil where it has none.
func (db *Database) AppKeyForServer(wg Workgroup, serverName string) (types.PrivateKey, error) {
	var appKey types.PrivateKey
	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT c.app_key
			FROM connections c
			JOIN shares s ON s.share_name = c.share_name
			WHERE c.workgroup = $1
			AND s.server_name = $2
			AND s.share_type = 'indexd'
			AND c.app_key IS NOT NULL
			LIMIT 1
		`
		err := tx.QueryRow(ctx, query, wg.ID, serverName).Scan(&appKey)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("failed to retrieve the app key of the workgroup: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return appKey, nil
}

// KeyedConnection is a connection that holds an app key: which workgroup is on
// which indexd share, and where that share's indexer is.
type KeyedConnection struct {
	Workgroup Workgroup
	Share     string
	Server    string
}

// KeyedConnections returns every connection that holds an app key, which is what
// an import of an indexd account can be read with instead of a pasted key.
func (db *Database) KeyedConnections() (conns []KeyedConnection, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT w.id, w.uuid, w.name, c.share_name, s.server_name
			FROM connections c
			JOIN shares s ON s.share_name = c.share_name
			JOIN workgroups w ON w.id = c.workgroup
			WHERE s.share_type = 'indexd'
			AND c.app_key IS NOT NULL
			ORDER BY c.share_name, w.id
		`
		rows, err := tx.Query(ctx, query)
		if err != nil {
			return fmt.Errorf("failed to retrieve the keyed connections: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var conn KeyedConnection
			var name *string
			if err := rows.Scan(&conn.Workgroup.ID, &conn.Workgroup.UUID, &name, &conn.Share, &conn.Server); err != nil {
				return fmt.Errorf("failed to scan a keyed connection: %w", err)
			}
			if name != nil {
				conn.Workgroup.Name = *name
			}
			conns = append(conns, conn)
		}

		return rows.Err()
	})
	if err != nil {
		return nil, err
	}

	return conns, nil
}

// AppKeyHolders returns the workgroups that hold an app key for the given server,
// for the connections that are made from another workgroup's key.
func (db *Database) AppKeyHolders(serverName string) (wgs []Workgroup, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT DISTINCT w.id, w.uuid, w.name
			FROM connections c
			JOIN shares s ON s.share_name = c.share_name
			JOIN workgroups w ON w.id = c.workgroup
			WHERE s.server_name = $1
			AND s.share_type = 'indexd'
			AND c.app_key IS NOT NULL
			ORDER BY w.id
		`
		rows, err := tx.Query(ctx, query, serverName)
		if err != nil {
			return fmt.Errorf("failed to retrieve the holders of an app key: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var id int
			var u uuid.UUID
			var name *string
			if err := rows.Scan(&id, &u, &name); err != nil {
				return fmt.Errorf("failed to scan the holder of an app key: %w", err)
			}
			wg := Workgroup{ID: id, UUID: u}
			if name != nil {
				wg.Name = *name
			}
			wgs = append(wgs, wg)
		}

		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return
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
