package stores

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Link is a sharing link to a file: whoever holds the token reads the file as
// the account that made the link. ExpiresAt is zero for a link that lasts.
type Link struct {
	Token     string
	Share     string
	Path      string
	Account   Account
	CreatedAt time.Time
	ExpiresAt time.Time
}

// newToken is a token nobody guesses: 32 random bytes, URL-safe.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to make a token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// CreateLink makes a link to the file on the share for the account, expiring
// at the time unless it is zero, and returns it.
func (db *Database) CreateLink(share, path string, acc Account, expires time.Time) (Link, error) {
	token, err := newToken()
	if err != nil {
		return Link{}, err
	}
	l := Link{Token: token, Share: share, Path: normalizePath(path), Account: acc, ExpiresAt: expires}
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		var at *time.Time
		if !expires.IsZero() {
			at = &expires
		}
		const insert = `
			INSERT INTO links (token, share_name, path, account, expires_at)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING created_at
		`
		if err := tx.QueryRow(ctx, insert, l.Token, l.Share, l.Path, acc.ID, at).Scan(&l.CreatedAt); err != nil {
			return fmt.Errorf("failed to create the link: %w", err)
		}
		return nil
	})
	return l, err
}

const linkColumns = `
	l.token, l.share_name, l.path, l.created_at, COALESCE(l.expires_at, 'epoch'::timestamptz),
	a.id, a.account_name, w.uuid
	FROM links l
	JOIN accounts a ON a.id = l.account
	JOIN workgroups w ON w.id = a.workgroup
`

// scanLink reads a link with its account from a row of linkColumns.
func scanLink(row pgx.Row) (Link, error) {
	var l Link
	var u uuid.UUID
	if err := row.Scan(&l.Token, &l.Share, &l.Path, &l.CreatedAt, &l.ExpiresAt, &l.Account.ID, &l.Account.Username, &u); err != nil {
		return Link{}, err
	}
	if l.ExpiresAt.Unix() == 0 {
		l.ExpiresAt = time.Time{}
	}
	l.Account.Workgroup = u.String()
	return l, nil
}

// Link is the link with the token, expired or not, or ErrNotFound.
func (db *Database) Link(token string) (l Link, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		l, err = scanLink(tx.QueryRow(ctx, "SELECT "+linkColumns+" WHERE l.token = $1", token))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("failed to find the link: %w", err)
		}
		return nil
	})
	return
}

// Links lists the links to the file on the share made by the accounts of the
// account's workgroup, newest first.
func (db *Database) Links(share, path string, acc Account) (links []Link, err error) {
	u, err := uuid.Parse(acc.Workgroup)
	if err != nil {
		return nil, fmt.Errorf("invalid workgroup UUID: %w", err)
	}
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT "+linkColumns+" WHERE l.share_name = $1 AND l.path = $2 AND w.uuid = $3 ORDER BY l.created_at DESC, l.token", share, normalizePath(path), u[:])
		if err != nil {
			return fmt.Errorf("failed to list the links: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			l, err := scanLink(rows)
			if err != nil {
				return fmt.Errorf("failed to scan the link: %w", err)
			}
			links = append(links, l)
		}
		return rows.Err()
	})
	return
}

// DeleteLink deletes the link with the token, where an account of the
// account's workgroup made it, and returns ErrNotFound otherwise.
func (db *Database) DeleteLink(token string, acc Account) error {
	u, err := uuid.Parse(acc.Workgroup)
	if err != nil {
		return fmt.Errorf("invalid workgroup UUID: %w", err)
	}
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const del = `
			DELETE FROM links l
			USING accounts a, workgroups w
			WHERE l.token = $1 AND a.id = l.account AND w.id = a.workgroup AND w.uuid = $2
		`
		tag, err := tx.Exec(ctx, del, token, u[:])
		if err != nil {
			return fmt.Errorf("failed to delete the link: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ExpireLinks drops the links past their time, and returns how many went.
func (db *Database) ExpireLinks() (dropped int, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "DELETE FROM links WHERE expires_at < NOW()")
		if err != nil {
			return fmt.Errorf("failed to expire the links: %w", err)
		}
		dropped = int(tag.RowsAffected())
		return nil
	})
	return
}

// The Lite mode keeps no links.
func (js *JSONStore) CreateLink(string, string, Account, time.Time) (Link, error) {
	return Link{}, ErrLiteMode
}
func (js *JSONStore) Link(string) (Link, error)                     { return Link{}, ErrLiteMode }
func (js *JSONStore) Links(string, string, Account) ([]Link, error) { return nil, ErrLiteMode }
func (js *JSONStore) DeleteLink(string, Account) error              { return ErrLiteMode }
