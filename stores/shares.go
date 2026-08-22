package stores

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Share represents a renterd bucket, which is mounted as a remote share.
//
// AllowGuest lets the passwordless accounts of a workgroup connect to the
// share, and AllowAnonymous lets clients that present no credentials at all
// connect, which the server-wide Anonymous setting has to permit as well.
// PublicDir is the one folder an anonymous session may use, and is what its
// uploads are visible in.
type Share struct {
	Name           string    `json:"name"`
	Type           string    `json:"type"`
	ServerName     string    `json:"serverName"`
	Password       string    `json:"password,omitempty"`
	Bucket         string    `json:"bucket,omitempty"`
	Remark         string    `json:"remark,omitempty"`
	CreatedAt      time.Time `json:"createdAt,omitempty"`
	DataShards     uint8     `json:"dataShards,omitempty"`
	ParityShards   uint8     `json:"parityShards,omitempty"`
	AllowGuest     bool      `json:"allowGuest,omitempty"`
	AllowAnonymous bool      `json:"allowAnonymous,omitempty"`
	PublicDir      string    `json:"publicDir,omitempty"`
}

// RegisterShare registers a new share in the database.
func (db *Database) RegisterShare(s Share) error {
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			INSERT INTO shares (
				share_name,
				share_type,
				server_name,
				api_password,
				bucket,
				remark,
				created_at,
				data_shards,
				parity_shards,
				allow_guest,
				allow_anonymous,
				public_dir
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`
		_, err := tx.Exec(ctx, query, s.Name, s.Type, s.ServerName, s.Password, s.Bucket, s.Remark, time.Now(), s.DataShards, s.ParityShards, s.AllowGuest, s.AllowAnonymous, s.PublicDir)
		if err != nil {
			return fmt.Errorf("failed to register share: %w", err)
		} else if err := db.shares.RegisterShare(s); err != nil {
			return fmt.Errorf("failed to add share: %w", err)
		} else {
			return nil
		}
	})
}

// UnregisterShare removes the share from the database.
func (db *Database) UnregisterShare(name string) error {
	if name == "" {
		return nil
	}

	s, err := db.GetShare(name)
	if err != nil {
		return err
	} else if s.Name == "" {
		return nil
	}

	if err := db.shares.RemoveShare(s); err != nil {
		return fmt.Errorf("failed to close share: %w", err)
	}

	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			DELETE FROM shares
			WHERE share_name = $1
		`
		_, err := tx.Exec(ctx, query, name)
		if err != nil {
			return fmt.Errorf("failed to remove share: %w", err)
		} else {
			return nil
		}
	})
}

// GetShare tries to retrieve the share information by its name.
func (db *Database) GetShare(name string) (s Share, err error) {
	if name == "" {
		return Share{}, nil
	}
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT
				share_type,
				server_name,
				api_password,
				bucket,
				remark,
				created_at,
				data_shards,
				parity_shards,
				allow_guest,
				allow_anonymous,
				public_dir
			FROM shares
			WHERE share_name = $1
		`
		var backend, server, password, bucket, remark, publicDir string
		var created time.Time
		var dataShards, parityShards int
		var allowGuest, allowAnonymous bool
		err = tx.QueryRow(ctx, query, name).Scan(&backend, &server, &password, &bucket, &remark, &created, &dataShards, &parityShards, &allowGuest, &allowAnonymous, &publicDir)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("failed to retrieve share: %w", err)
		}
		s = Share{
			Name:           name,
			Type:           backend,
			ServerName:     server,
			Password:       password,
			Bucket:         bucket,
			Remark:         remark,
			CreatedAt:      created,
			DataShards:     uint8(dataShards),
			ParityShards:   uint8(parityShards),
			AllowGuest:     allowGuest,
			AllowAnonymous: allowAnonymous,
			PublicDir:      publicDir,
		}
		return nil
	})
	return
}

// shareColumns is what a share is read back from, aliased to s, in the order
// scanShares takes them.
const shareColumns = `
	s.share_name,
	s.share_type,
	s.server_name,
	s.api_password,
	s.bucket,
	s.remark,
	s.created_at,
	s.data_shards,
	s.parity_shards,
	s.allow_guest,
	s.allow_anonymous,
	s.public_dir
`

// scanShares reads the rows of a query that selects shareColumns.
func scanShares(rows pgx.Rows) (shares []Share, err error) {
	defer rows.Close()

	for rows.Next() {
		var s Share
		var dataShards, parityShards int
		if err := rows.Scan(
			&s.Name,
			&s.Type,
			&s.ServerName,
			&s.Password,
			&s.Bucket,
			&s.Remark,
			&s.CreatedAt,
			&dataShards,
			&parityShards,
			&s.AllowGuest,
			&s.AllowAnonymous,
			&s.PublicDir,
		); err != nil {
			return nil, fmt.Errorf("failed to retrieve share: %w", err)
		}
		s.DataShards = uint8(dataShards)
		s.ParityShards = uint8(parityShards)
		shares = append(shares, s)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate shares: %w", err)
	}
	return shares, nil
}

// GetShares lists all shares the specified account has access to.
func (db *Database) GetShares(acc Account) (shares []Share, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT DISTINCT ` + shareColumns + `
			FROM shares AS s
			JOIN policies AS p
			ON p.share_name = s.share_name
			WHERE p.account = $1
			AND (p.read_access
			OR p.write_access
			OR p.delete_access
			OR p.execute_access)
		`
		rows, err := tx.Query(ctx, query, acc.ID)
		if err != nil {
			return fmt.Errorf("failed to retrieve share: %w", err)
		}
		shares, err = scanShares(rows)
		return err
	})
	return
}

// GetAllShares lists all registered shares.
func (db *Database) GetAllShares() (shares []Share, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT ` + shareColumns + `
			FROM shares AS s
			ORDER BY s.share_name
		`
		rows, err := tx.Query(ctx, query)
		if err != nil {
			return fmt.Errorf("failed to retrieve shares: %w", err)
		}
		shares, err = scanShares(rows)
		return err
	})
	return
}

// GetAccounts lists all the accounts that can connect to the specified share.
func (db *Database) GetAccounts(sh Share) (ars []AccessRights, err error) {
	if sh.Name == "" {
		return nil, nil
	}
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT account, read_access, write_access, delete_access, execute_access
			FROM policies
			WHERE share_name = $1
		`
		rows, err := tx.Query(ctx, query, sh.Name)
		if err != nil {
			return fmt.Errorf("failed to retrieve policies: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var accountID int
			var read, write, delete, execute bool
			if err := rows.Scan(&accountID, &read, &write, &delete, &execute); err != nil {
				return fmt.Errorf("failed to retrieve policies: %w", err)
			}
			ars = append(ars, AccessRights{
				ShareName:     sh.Name,
				AccountID:     accountID,
				ReadAccess:    read,
				WriteAccess:   write,
				DeleteAccess:  delete,
				ExecuteAccess: execute,
			})
		}
		return nil
	})
	return
}
