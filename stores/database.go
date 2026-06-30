package stores

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.sia.tech/core/types"
)

// Database represents a PostgreSQL-backed store.
type Database struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	shares Shares
}

// Shares is the minimal interface of the share manager.
type Shares interface {
	RegisterShare(sh Share) error
	RemoveShare(sh Share) error
	UpdateAccessRights(ss Share, ar AccessRights) error
	RemoveAccess(acc Account)
	AddConnection(wg Workgroup, share Share, appKey types.PrivateKey) error
	RemoveConnection(wg Workgroup, share Share) error
}

// Close closes the underlying database connection.
func (db *Database) Close() {
	db.pool.Close()
}

// NewStore returns an initialized Database instance.
func NewStore(ctx context.Context, dc DatabaseConfig) (*Database, error) {
	pool, err := pgxpool.New(ctx, dc.String())
	if err != nil {
		return nil, fmt.Errorf("failed to create pool: %w", err)
	} else if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	log.Printf("Connected to SQL database %s, %s:%d\n", dc.Database, dc.Host, dc.Port)
	return &Database{
		ctx:  ctx,
		pool: pool,
	}, nil
}

// WithShares adds a share manager to the Database.
func (db *Database) WithShares(shares Shares) {
	db.shares = shares
}

// txn executes a statement within an SQL transaction.
func (db *Database) txn(fn func(context.Context, pgx.Tx) error) error {
	tx, err := db.pool.BeginTx(db.ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(db.ctx)

	if err := fn(db.ctx, tx); err != nil {
		return err
	} else if err := tx.Commit(db.ctx); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}
