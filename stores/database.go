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
	// ctx spans the lifetime of the store and is deliberately not derived from
	// the context that opened it: that one is the process' signal context, and
	// tying the transactions to it would fail every database call the moment a
	// shutdown starts — including the ones a graceful shutdown depends on, like
	// requeueing an upload that was cut short. Only Close ends it.
	ctx    context.Context
	cancel context.CancelFunc
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

// Close closes the underlying database connection. Whatever is still running a
// transaction by then is cut off, so the callers that need to finish their work
// have to be stopped first.
func (db *Database) Close() {
	db.cancel()
	db.pool.Close()
}

// NewStore returns an initialized Database instance. The given context only
// bounds the connection setup; the store stays usable until it is closed.
func NewStore(ctx context.Context, dc DatabaseConfig) (*Database, error) {
	pool, err := pgxpool.New(ctx, dc.String())
	if err != nil {
		return nil, fmt.Errorf("failed to create pool: %w", err)
	} else if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	log.Printf("Connected to SQL database %s, %s:%d\n", dc.Database, dc.Host, dc.Port)
	lifetime, cancel := context.WithCancel(context.Background())
	return &Database{
		ctx:    lifetime,
		cancel: cancel,
		pool:   pool,
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
