package stores

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.sia.tech/core/types"
)

type noopShares struct{}

func (noopShares) RegisterShare(sh Share) error                                      { return nil }
func (noopShares) RemoveShare(sh Share) error                                        { return nil }
func (noopShares) UpdateAccessRights(sh Share, ar AccessRights) error                { return nil }
func (noopShares) RemoveAccess(acc Account)                                          {}
func (noopShares) AddConnection(_ Workgroup, _ Share, _ types.PrivateKey) error      { return nil }
func (noopShares) RemoveConnection(_ Workgroup, _ Share) error                       { return nil }

func NewTestStore(t *testing.T, ctx context.Context) *Database {
	return NewTestStoreNamed(t, ctx, envOr(t, "TEST_DB_NAME", "sombrero_test"))
}

func NewTestStoreNamed(t *testing.T, ctx context.Context, dbName string) *Database {
	t.Helper()

	cfg := DatabaseConfig{
		Host:     envOr(t, "TEST_DB_HOST", "127.0.0.1"),
		Port:     envOrInt(t, "TEST_DB_PORT", 5432),
		User:     envOr(t, "TEST_DB_USER", "postgres"),
		Password: os.Getenv("TEST_DB_PASSWORD"),
		Database: dbName,
		SSLMode:  envOr(t, "TEST_DB_SSLMODE", "disable"),
	}

	db, err := NewStore(ctx, cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	db.WithShares(noopShares{})

	resetDatabaseFromInitSQL(t, db)
	return db
}

func resetDatabaseFromInitSQL(t *testing.T, db *Database) {
	t.Helper()

	initSQLPath := envOr(t, "TEST_INIT_SQL", findInitSQL(t))
	sqlBytes, err := os.ReadFile(initSQLPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", initSQLPath, err)
	}
	initSQL := string(sqlBytes)

	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const resetSQL = `
			DROP SCHEMA public CASCADE;
			CREATE SCHEMA public;
		`

		if _, err := tx.Exec(ctx, resetSQL); err != nil {
			return fmt.Errorf("reset schema: %w", err)
		}

		if _, err := tx.Exec(ctx, initSQL); err != nil {
			return fmt.Errorf("apply init.sql: %w", err)
		}

		return nil
	})

	if err != nil {
		t.Fatalf("resetDatabaseFromInitSQL: %v", err)
	}
}

func envOr(t *testing.T, key, fallback string) string {
	t.Helper()
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envOrInt(t *testing.T, key string, fallback int) int {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	var n int
	_, err := fmt.Sscanf(v, "%d", &n)
	if err != nil {
		t.Fatalf("invalid %s: %q", key, v)
	}
	return n
}

func findInitSQL(t *testing.T) string {
	t.Helper()

	candidates := []string{
		"init.sql",
		filepath.Join("..", "init.sql"),
		filepath.Join("..", "..", "init.sql"),
	}

	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	t.Fatalf("couldn't find init.sql; set TEST_INIT_SQL")
	return ""
}
