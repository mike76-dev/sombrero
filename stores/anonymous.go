package stores

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AnonymousWorkgroup is the workgroup that owns what anonymous sessions
// upload. It belongs to the server rather than to anybody's users: the nil UUID
// cannot collide with a generated one, and no client may log in under it.
var AnonymousWorkgroup = uuid.UUID{}

// AnonymousAccount is the name of the account an anonymous session acts as. It
// has no password, like the guest accounts, but unlike them it is not reachable
// by logging in: only a session that presented no credentials at all is bound
// to it.
const AnonymousAccount = "anonymous"

// GuestAccount is the name a client sends when its user ticks a guest box. The
// dialogs that offer one ask for nothing else: macOS has no field for a
// workgroup there, so a login under this name may match no account at all.
const GuestAccount = "guest"

// ErrReservedWorkgroup is returned when something tries to create a workgroup
// or an account where only the server's own anonymous identity belongs.
var ErrReservedWorkgroup = errors.New("the anonymous workgroup is reserved")

// IsAnonymousWorkgroup reports whether the workgroup UUID, in whatever spelling
// it arrived, is the reserved one.
func IsAnonymousWorkgroup(workgroup string) bool {
	u, err := uuid.Parse(workgroup)
	return err == nil && u == AnonymousWorkgroup
}

// EnsureAnonymous creates the reserved workgroup and the account in it, unless
// they are there already, and returns the account. It is what the server calls
// when it is configured to admit anonymous sessions.
func (db *Database) EnsureAnonymous() (acc Account, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const addWorkgroup = `
			INSERT INTO workgroups (uuid)
			VALUES ($1)
			ON CONFLICT DO NOTHING
		`
		if _, err := tx.Exec(ctx, addWorkgroup, AnonymousWorkgroup[:]); err != nil {
			return fmt.Errorf("failed to add the anonymous workgroup: %w", err)
		}

		const addAccount = `
			INSERT INTO accounts (account_name, password_hash, workgroup)
			SELECT $1, $2, w.id
			FROM workgroups w
			WHERE w.uuid = $3
			ON CONFLICT DO NOTHING
		`
		if _, err := tx.Exec(ctx, addAccount, AnonymousAccount, emptyNTHash, AnonymousWorkgroup[:]); err != nil {
			return fmt.Errorf("failed to add the anonymous account: %w", err)
		}

		const lookup = `
			SELECT a.id, a.password_hash
			FROM accounts a
			JOIN workgroups w ON w.id = a.workgroup
			WHERE a.account_name = $1
				AND w.uuid = $2
		`
		var id int
		var pwh []byte
		if err := tx.QueryRow(ctx, lookup, AnonymousAccount, AnonymousWorkgroup[:]).Scan(&id, &pwh); err != nil {
			return fmt.Errorf("failed to retrieve the anonymous account: %w", err)
		}

		acc = Account{
			ID:        id,
			Username:  AnonymousAccount,
			NTHash:    pwh,
			Workgroup: AnonymousWorkgroup.String(),
		}
		return nil
	})
	return
}

// EnsureAnonymous creates the reserved workgroup and the account in it, unless
// they are there already, and returns the account.
func (js *JSONStore) EnsureAnonymous() (acc Account, err error) {
	err = js.update(func(d *jsonData) error {
		workgroup := AnonymousWorkgroup.String()

		var found bool
		for _, wg := range d.Workgroups {
			if wg.UUID == AnonymousWorkgroup {
				found = true
				break
			}
		}
		if !found {
			d.Workgroups = append(d.Workgroups, Workgroup{
				ID:   d.NextWorkgroupID,
				UUID: AnonymousWorkgroup,
			})
			d.NextWorkgroupID++
		}

		for _, a := range d.Accounts {
			if a.Username == AnonymousAccount && a.Workgroup == workgroup {
				acc = Account{ID: a.ID, Username: a.Username, NTHash: a.NTHash, Workgroup: a.Workgroup}
				return nil
			}
		}

		acc = Account{
			ID:        d.NextAccountID,
			Username:  AnonymousAccount,
			NTHash:    ntHash(""),
			Workgroup: workgroup,
		}
		d.Accounts = append(d.Accounts, jsonAccount{
			ID:        acc.ID,
			Username:  acc.Username,
			NTHash:    acc.NTHash,
			Workgroup: acc.Workgroup,
		})
		d.NextAccountID++
		return nil
	}, nil)
	return
}

// EnsurePublicDir makes the folder an anonymous session is confined to on the
// given share, owned by the reserved identity and open to everyone. It is what
// makes the folder visible to both sides: the anonymous session owns it, and
// every member of the share sees it for being owned by that identity.
//
// A folder of that name that somebody else already made is left alone and
// reported: taking it over would hand one workgroup's directory to everyone.
func (db *Database) EnsurePublicDir(share, name string) error {
	if share == "" || name == "" {
		return nil
	}

	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const create = `
			WITH anon AS (
				SELECT a.id AS account, a.workgroup
				FROM accounts a
				JOIN workgroups w ON w.id = a.workgroup
				WHERE a.account_name = $2
					AND w.uuid = $3
			)
			INSERT INTO directories (
				share_name,
				parent_id,
				name,
				full_path,
				account,
				workgroup,
				private,
				read_only
			)
			SELECT $1, NULL, $4, '/' || $4, anon.account, anon.workgroup, FALSE, FALSE
			FROM anon
			ON CONFLICT (share_name, full_path) DO NOTHING
		`

		if _, err := tx.Exec(ctx, create, share, AnonymousAccount, AnonymousWorkgroup[:], name); err != nil {
			return fmt.Errorf("failed to create the public folder: %w", err)
		}

		// Whether it was made here or was there already, it is only usable if
		// the reserved identity owns it.
		const owner = `
			SELECT w.uuid
			FROM directories d
			JOIN accounts a ON a.id = d.account
			JOIN workgroups w ON w.id = a.workgroup
			WHERE d.share_name = $1
				AND d.full_path = '/' || $2
		`

		var u []byte
		if err := tx.QueryRow(ctx, owner, share, name).Scan(&u); err != nil {
			return fmt.Errorf("failed to check the public folder: %w", err)
		}
		if !bytes.Equal(u, AnonymousWorkgroup[:]) {
			return fmt.Errorf("the public folder %q of share %s belongs to another workgroup", name, share)
		}

		return nil
	})
}

// EnsurePublicDir has nothing to do in the Lite mode: a renterd share keeps no
// folders of its own, and an anonymous session there is confined by its path
// alone.
func (js *JSONStore) EnsurePublicDir(share, name string) error {
	return nil
}
