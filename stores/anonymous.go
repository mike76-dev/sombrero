package stores

import (
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
