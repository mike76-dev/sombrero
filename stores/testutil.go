package stores

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.sia.tech/core/types"
)

type noopShares struct{}

func (noopShares) RegisterShare(sh Share) error                                 { return nil }
func (noopShares) UpdateShare(sh Share) error                                   { return nil }
func (noopShares) RemoveShare(sh Share) error                                   { return nil }
func (noopShares) UpdateAccessRights(sh Share, ar AccessRights) error           { return nil }
func (noopShares) RemoveAccess(acc Account)                                     {}
func (noopShares) AddConnection(_ Workgroup, _ Share, _ types.PrivateKey) error { return nil }
func (noopShares) RemoveConnection(_ Workgroup, _ Share) error                  { return nil }

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

	// Emptied before NewStore, which would refuse a schema a test left behind,
	// and which then creates the tables the way the server does.
	resetDatabase(t, ctx, cfg)

	db, err := NewStore(ctx, cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	db.WithShares(noopShares{})
	return db
}

func resetDatabase(t *testing.T, ctx context.Context, cfg DatabaseConfig) {
	t.Helper()

	conn, err := pgx.Connect(ctx, cfg.String())
	if err != nil {
		t.Fatalf("resetDatabase: %v", err)
	}
	defer conn.Close(ctx)

	const resetSQL = `
		DROP SCHEMA public CASCADE;
		CREATE SCHEMA public;
	`
	if _, err := conn.Exec(ctx, resetSQL); err != nil {
		t.Fatalf("resetDatabase: %v", err)
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
