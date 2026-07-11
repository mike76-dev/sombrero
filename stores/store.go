package stores

import (
	"github.com/google/uuid"
	"go.sia.tech/core/types"
)

// Store is the interface shared by the PostgreSQL-backed store used in the
// Normal mode and the JSON-backed store used in the Lite mode.
type Store interface {
	IsBanned(host string) (bool, string, error)
	BanHost(host, reason string) error
	UnbanHost(host string) error
	ClearBans() error

	GetAccountByID(id int) (acc Account, err error)
	FindAccount(username, workgroup string) (acc Account, err error)
	AddAccount(acc Account) error
	HasAccount(username, workgroup string) (bool, error)
	RemoveAccount(username, workgroup string) error
	FindAccounts(workgroup string) (accs []Account, err error)
	RemoveAccounts(workgroup string) error

	GetWorkgroupByID(id int) (Workgroup, error)
	FindWorkgroup(u uuid.UUID) (Workgroup, error)
	FindWorkgroupByName(name string) (Workgroup, error)
	GetWorkgroups() ([]Workgroup, error)
	AddWorkgroup(wg Workgroup) error
	UpdateWorkgroup(wg Workgroup) error
	RemoveWorkgroup(wg Workgroup) error

	GetAccessRights(share Share, acc Account) (ar AccessRights, err error)
	SetAccessRights(ar AccessRights) error
	RemoveAccessRights(share Share, acc Account) error
	ClearAccessRights(acc Account) error

	RegisterShare(s Share) error
	UnregisterShare(name string) error
	GetShare(name string) (s Share, err error)
	GetShares(acc Account) (shares []Share, err error)
	GetAllShares() (shares []Share, err error)
	GetAccounts(sh Share) (ars []AccessRights, err error)

	AddConnection(wg Workgroup, share Share, appKey types.PrivateKey) error
	RemoveConnection(wg Workgroup, share Share) error
	IsConnected(wg Workgroup, share Share) (bool, types.PrivateKey, error)
	SetAppKey(wg Workgroup, share Share, key types.PrivateKey) error

	WithShares(shares Shares)
	Close()
}

var (
	_ Store = (*Database)(nil)
	_ Store = (*JSONStore)(nil)
)
