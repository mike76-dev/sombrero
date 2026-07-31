package stores

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mike76-dev/sombrero/utils"
	"go.sia.tech/core/types"
	"golang.org/x/crypto/md4"
)

// jsonStoreFilename is the name of the persistence file of a JSONStore.
const jsonStoreFilename = "store.json"

// ErrLiteMode is returned when an operation is not available in the Lite mode.
var ErrLiteMode = errors.New("not supported in Lite mode")

// jsonAccount is the persisted form of an Account. Unlike Account, it never
// carries the plaintext password, only the NT hash.
type jsonAccount struct {
	ID        int    `json:"id"`
	Username  string `json:"username"`
	NTHash    []byte `json:"ntHash"`
	Workgroup string `json:"workgroup"` // workgroup UUID
}

// jsonPolicy is the persisted form of an AccessRights entry.
type jsonPolicy struct {
	ShareName     string `json:"shareName"`
	AccountID     int    `json:"accountID"`
	Workgroup     string `json:"workgroup"` // workgroup UUID
	ReadAccess    bool   `json:"readAccess"`
	WriteAccess   bool   `json:"writeAccess"`
	DeleteAccess  bool   `json:"deleteAccess"`
	ExecuteAccess bool   `json:"executeAccess"`
}

// jsonConnection is the persisted form of a workgroup-share connection.
type jsonConnection struct {
	Workgroup string           `json:"workgroup"` // workgroup UUID
	ShareName string           `json:"shareName"`
	AppKey    types.PrivateKey `json:"appKey,omitempty"`
}

// jsonBan is the persisted form of a ban list entry.
type jsonBan struct {
	Host   string `json:"host"`
	Reason string `json:"reason"`
}

// jsonData is the root object of the persistence file.
type jsonData struct {
	Version         int              `json:"version"`
	NextWorkgroupID int              `json:"nextWorkgroupID"`
	NextAccountID   int              `json:"nextAccountID"`
	Shares          []Share          `json:"shares"`
	Workgroups      []Workgroup      `json:"workgroups"`
	Accounts        []jsonAccount    `json:"accounts"`
	Policies        []jsonPolicy     `json:"policies"`
	Connections     []jsonConnection `json:"connections"`
	Bans            []jsonBan        `json:"bans"`
}

// clone returns a deep copy of the data, used to roll back failed updates.
func (d jsonData) clone() jsonData {
	d.Shares = append([]Share(nil), d.Shares...)
	d.Workgroups = append([]Workgroup(nil), d.Workgroups...)
	d.Accounts = append([]jsonAccount(nil), d.Accounts...)
	d.Policies = append([]jsonPolicy(nil), d.Policies...)
	d.Connections = append([]jsonConnection(nil), d.Connections...)
	d.Bans = append([]jsonBan(nil), d.Bans...)
	return d
}

// JSONStore is a file-backed store used in the Lite mode. It only supports
// renterd shares and doesn't require a PostgreSQL database.
type JSONStore struct {
	mu     sync.Mutex
	path   string
	shares Shares
	data   jsonData
}

// NewJSONStore loads a JSONStore from the specified directory, creating an
// empty one if no persistence file exists yet.
func NewJSONStore(dir string) (*JSONStore, error) {
	js := &JSONStore{
		path: filepath.Join(dir, jsonStoreFilename),
		data: jsonData{
			Version:         1,
			NextWorkgroupID: 1,
			NextAccountID:   1,
		},
	}

	b, err := os.ReadFile(js.path)
	if errors.Is(err, os.ErrNotExist) {
		if err := js.save(); err != nil {
			return nil, err
		}
		log.Printf("Created JSON store at %s\n", js.path)
		return js, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to read store file: %w", err)
	}

	if err := json.Unmarshal(b, &js.data); err != nil {
		return nil, fmt.Errorf("failed to decode store file: %w", err)
	}
	if js.data.NextWorkgroupID < 1 {
		js.data.NextWorkgroupID = 1
	}
	if js.data.NextAccountID < 1 {
		js.data.NextAccountID = 1
	}

	log.Printf("Loaded JSON store from %s\n", js.path)
	return js, nil
}

// WithShares adds a share manager to the JSONStore.
func (js *JSONStore) WithShares(shares Shares) {
	js.shares = shares
}

// Close implements Store. A JSONStore is saved on every mutation,
// so there is nothing to do here.
func (js *JSONStore) Close() {}

// save persists the data to disk. The caller must hold the mutex.
func (js *JSONStore) save() error {
	b, err := json.MarshalIndent(js.data, "", "\t")
	if err != nil {
		return fmt.Errorf("failed to encode store file: %w", err)
	}

	tmp := js.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to create store file: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("failed to write store file: %w", err)
	} else if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("failed to sync store file: %w", err)
	} else if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close store file: %w", err)
	}

	if err := os.Rename(tmp, js.path); err != nil {
		return fmt.Errorf("failed to replace store file: %w", err)
	}
	return nil
}

// update applies a mutation and persists it. If notify is not nil, it is
// called after the mutation without holding the mutex (the share manager
// calls back into the store); if it fails, the mutation is rolled back,
// mirroring the transactional behavior of the SQL store.
func (js *JSONStore) update(mutate func(*jsonData) error, notify func() error) error {
	js.mu.Lock()
	snapshot := js.data.clone()
	if err := mutate(&js.data); err != nil {
		js.data = snapshot
		js.mu.Unlock()
		return err
	}
	if err := js.save(); err != nil {
		js.data = snapshot
		js.mu.Unlock()
		return err
	}
	js.mu.Unlock()

	if notify == nil {
		return nil
	}
	if err := notify(); err != nil {
		js.mu.Lock()
		js.data = snapshot
		if saveErr := js.save(); saveErr != nil {
			log.Printf("failed to roll back store file: %v", saveErr)
		}
		js.mu.Unlock()
		return err
	}
	return nil
}

// IsBanned returns true if the remote host is banned. The ban reason is also returned.
func (js *JSONStore) IsBanned(host string) (bool, string, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, ban := range js.data.Bans {
		if ban.Host == host {
			return true, ban.Reason, nil
		}
	}
	return false, "", nil
}

// BanHost puts the host on the ban list.
func (js *JSONStore) BanHost(host, reason string) error {
	return js.update(func(d *jsonData) error {
		for _, ban := range d.Bans {
			if ban.Host == host {
				return nil
			}
		}
		d.Bans = append(d.Bans, jsonBan{Host: host, Reason: reason})
		return nil
	}, nil)
}

// UnbanHost removes the host from the ban list.
func (js *JSONStore) UnbanHost(host string) error {
	return js.update(func(d *jsonData) error {
		for i, ban := range d.Bans {
			if ban.Host == host {
				d.Bans = append(d.Bans[:i], d.Bans[i+1:]...)
				return nil
			}
		}
		return nil
	}, nil)
}

// ClearBans clears the ban list.
func (js *JSONStore) ClearBans() error {
	return js.update(func(d *jsonData) error {
		d.Bans = nil
		return nil
	}, nil)
}

// GetAccountByID tries to retrieve the account by its ID.
func (js *JSONStore) GetAccountByID(id int) (acc Account, err error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, a := range js.data.Accounts {
		if a.ID == id {
			return Account{ID: a.ID, Username: a.Username, NTHash: a.NTHash, Workgroup: a.Workgroup}, nil
		}
	}
	return Account{}, ErrAccountNotFound
}

// FindAccount tries to retrieve the account by the username and the workgroup UUID.
func (js *JSONStore) FindAccount(username, workgroup string) (acc Account, err error) {
	if _, err := uuid.Parse(workgroup); err != nil {
		return acc, fmt.Errorf("invalid workgroup UUID: %w", err)
	}
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, a := range js.data.Accounts {
		if a.Username == username && a.Workgroup == workgroup {
			return Account{ID: a.ID, Username: a.Username, NTHash: a.NTHash, Workgroup: a.Workgroup}, nil
		}
	}
	return Account{}, ErrAccountNotFound
}

// AddAccount adds a new account to the store.
func (js *JSONStore) AddAccount(acc Account) error {
	if _, err := uuid.Parse(acc.Workgroup); err != nil {
		return fmt.Errorf("invalid workgroup UUID: %w", err)
	}
	return js.update(func(d *jsonData) error {
		if _, ok := findWorkgroupByUUID(d, acc.Workgroup); !ok {
			return fmt.Errorf("failed to add account: workgroup not found")
		}
		for _, a := range d.Accounts {
			if a.Username == acc.Username && a.Workgroup == acc.Workgroup {
				return fmt.Errorf("failed to add account: account already exists")
			}
		}

		h := md4.New()
		h.Write(utils.EncodeStringToBytes(acc.Password))
		d.Accounts = append(d.Accounts, jsonAccount{
			ID:        d.NextAccountID,
			Username:  acc.Username,
			NTHash:    h.Sum(nil),
			Workgroup: acc.Workgroup,
		})
		d.NextAccountID++
		return nil
	}, nil)
}

// HasAccount returns true if there is such account in the store.
func (js *JSONStore) HasAccount(username, workgroup string) (bool, error) {
	if _, err := uuid.Parse(workgroup); err != nil {
		return false, fmt.Errorf("invalid workgroup UUID: %w", err)
	}
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, a := range js.data.Accounts {
		if a.Username == username && a.Workgroup == workgroup {
			return true, nil
		}
	}
	return false, nil
}

// RemoveAccount removes the specified account from the store
// together with its access policies.
func (js *JSONStore) RemoveAccount(username, workgroup string) error {
	if _, err := uuid.Parse(workgroup); err != nil {
		return fmt.Errorf("invalid workgroup UUID: %w", err)
	}
	return js.update(func(d *jsonData) error {
		for i, a := range d.Accounts {
			if a.Username == username && a.Workgroup == workgroup {
				d.Accounts = append(d.Accounts[:i], d.Accounts[i+1:]...)
				removePolicies(d, func(p jsonPolicy) bool { return p.AccountID == a.ID })
				return nil
			}
		}
		return nil
	}, func() error {
		js.shares.RemoveAccess(Account{Username: username, Workgroup: workgroup})
		return nil
	})
}

// FindAccounts returns all accounts of the specified workgroup.
func (js *JSONStore) FindAccounts(workgroup string) (accs []Account, err error) {
	if _, err := uuid.Parse(workgroup); err != nil {
		return nil, fmt.Errorf("invalid workgroup UUID: %w", err)
	}
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, a := range js.data.Accounts {
		if a.Workgroup == workgroup {
			accs = append(accs, Account{ID: a.ID, Username: a.Username, NTHash: a.NTHash, Workgroup: a.Workgroup})
		}
	}
	return
}

// RemoveAccounts removes all accounts of the specified workgroup
// together with their access policies.
func (js *JSONStore) RemoveAccounts(workgroup string) error {
	if _, err := uuid.Parse(workgroup); err != nil {
		return fmt.Errorf("invalid workgroup UUID: %w", err)
	}
	var removedAccs []Account
	return js.update(func(d *jsonData) error {
		removed := make(map[int]struct{})
		accs := d.Accounts[:0]
		for _, a := range d.Accounts {
			if a.Workgroup == workgroup {
				removed[a.ID] = struct{}{}
				removedAccs = append(removedAccs, Account{ID: a.ID, Username: a.Username, Workgroup: a.Workgroup})
			} else {
				accs = append(accs, a)
			}
		}
		d.Accounts = accs
		removePolicies(d, func(p jsonPolicy) bool {
			_, ok := removed[p.AccountID]
			return ok
		})
		return nil
	}, func() error {
		for _, acc := range removedAccs {
			js.shares.RemoveAccess(acc)
		}
		return nil
	})
}

// GetWorkgroupByID tries to retrieve the workgroup by its ID.
func (js *JSONStore) GetWorkgroupByID(id int) (wg Workgroup, err error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, w := range js.data.Workgroups {
		if w.ID == id {
			return w, nil
		}
	}
	return Workgroup{}, nil
}

// FindWorkgroup tries to retrieve the workgroup by its UUID.
func (js *JSONStore) FindWorkgroup(u uuid.UUID) (wg Workgroup, err error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	if w, ok := findWorkgroupByUUID(&js.data, u.String()); ok {
		return w, nil
	}
	return Workgroup{}, nil
}

// FindWorkgroupByName tries to retrieve the workgroup by its name.
func (js *JSONStore) FindWorkgroupByName(name string) (wg Workgroup, err error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, w := range js.data.Workgroups {
		if w.Name != "" && w.Name == name {
			return w, nil
		}
	}
	return Workgroup{}, nil
}

// GetWorkgroups lists all workgroups.
func (js *JSONStore) GetWorkgroups() (wgs []Workgroup, err error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	wgs = append(wgs, js.data.Workgroups...)
	sort.Slice(wgs, func(i, j int) bool { return wgs[i].ID < wgs[j].ID })
	return
}

// AddWorkgroup adds a new workgroup to the store.
func (js *JSONStore) AddWorkgroup(wg Workgroup) error {
	return js.update(func(d *jsonData) error {
		for _, w := range d.Workgroups {
			if w.UUID == wg.UUID {
				return fmt.Errorf("failed to add workgroup: workgroup already exists")
			}
			if wg.Name != "" && w.Name == wg.Name {
				return fmt.Errorf("failed to add workgroup: name already taken")
			}
		}
		wg.ID = d.NextWorkgroupID
		d.NextWorkgroupID++
		wg.PublicDirs = normalizePublicDirs(wg.PublicDirs)
		d.Workgroups = append(d.Workgroups, wg)
		return nil
	}, nil)
}

// UpdateWorkgroup replaces the public folders of a workgroup.
func (js *JSONStore) UpdateWorkgroup(wg Workgroup) error {
	return js.update(func(d *jsonData) error {
		for i, w := range d.Workgroups {
			if w.ID == wg.ID {
				d.Workgroups[i].PublicDirs = normalizePublicDirs(wg.PublicDirs)
				return nil
			}
		}
		return fmt.Errorf("workgroup not found")
	}, nil)
}

// RemoveWorkgroup removes the specified workgroup together with all
// associated accounts, connections, and access policies.
func (js *JSONStore) RemoveWorkgroup(wg Workgroup) error {
	var removedWG Workgroup
	var removedAccs []Account
	var shareNames []string
	return js.update(func(d *jsonData) error {
		for i, w := range d.Workgroups {
			if w.ID == wg.ID {
				removedWG = w
				u := w.UUID.String()
				d.Workgroups = append(d.Workgroups[:i], d.Workgroups[i+1:]...)
				accs := d.Accounts[:0]
				for _, a := range d.Accounts {
					if a.Workgroup == u {
						removedAccs = append(removedAccs, Account{ID: a.ID, Username: a.Username, Workgroup: a.Workgroup})
					} else {
						accs = append(accs, a)
					}
				}
				d.Accounts = accs
				conns := d.Connections[:0]
				for _, c := range d.Connections {
					if c.Workgroup == u {
						shareNames = append(shareNames, c.ShareName)
					} else {
						conns = append(conns, c)
					}
				}
				d.Connections = conns
				removePolicies(d, func(p jsonPolicy) bool { return p.Workgroup == u })
				return nil
			}
		}
		return nil
	}, func() error {
		for _, name := range shareNames {
			if err := js.shares.RemoveConnection(removedWG, Share{Name: name}); err != nil {
				return fmt.Errorf("failed to disconnect share: %w", err)
			}
		}
		for _, acc := range removedAccs {
			js.shares.RemoveAccess(acc)
		}
		return nil
	})
}

// GetAccessRights retrieves the access policy for the given account.
func (js *JSONStore) GetAccessRights(share Share, acc Account) (ar AccessRights, err error) {
	if share.Name == "" {
		return AccessRights{}, nil
	}
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, p := range js.data.Policies {
		if p.ShareName == share.Name && p.AccountID == acc.ID {
			return accessRightsFromPolicy(p), nil
		}
	}
	return AccessRights{}, nil
}

// SetAccessRights stores the access policy in the store.
// Returns an error if no connection exists between the account's workgroup and the share.
func (js *JSONStore) SetAccessRights(ar AccessRights) error {
	sh, err := js.GetShare(ar.ShareName)
	if err != nil {
		return fmt.Errorf("failed to retrieve share: %w", err)
	}
	return js.update(func(d *jsonData) error {
		if sh.Name == "" {
			return fmt.Errorf("failed to update policy: share not found")
		}
		var workgroup string
		for _, a := range d.Accounts {
			if a.ID == ar.AccountID {
				workgroup = a.Workgroup
				break
			}
		}
		if workgroup == "" {
			return fmt.Errorf("failed to update policy: account not found")
		}
		var connected bool
		for _, c := range d.Connections {
			if c.Workgroup == workgroup && c.ShareName == ar.ShareName {
				connected = true
				break
			}
		}
		if !connected {
			return fmt.Errorf("failed to update policy: no connection between the workgroup and the share")
		}

		policy := jsonPolicy{
			ShareName:     ar.ShareName,
			AccountID:     ar.AccountID,
			Workgroup:     workgroup,
			ReadAccess:    ar.ReadAccess,
			WriteAccess:   ar.WriteAccess,
			DeleteAccess:  ar.DeleteAccess,
			ExecuteAccess: ar.ExecuteAccess,
		}
		for i, p := range d.Policies {
			if p.ShareName == ar.ShareName && p.AccountID == ar.AccountID {
				d.Policies[i] = policy
				return nil
			}
		}
		d.Policies = append(d.Policies, policy)
		return nil
	}, func() error {
		if err := js.shares.UpdateAccessRights(sh, ar); err != nil {
			return fmt.Errorf("failed to update access rights: %w", err)
		}
		return nil
	})
}

// RemoveAccessRights removes the access policy to the share for the given account.
func (js *JSONStore) RemoveAccessRights(share Share, acc Account) error {
	if share.Name == "" {
		return nil
	}
	return js.update(func(d *jsonData) error {
		removePolicies(d, func(p jsonPolicy) bool {
			return p.ShareName == share.Name && p.AccountID == acc.ID
		})
		return nil
	}, func() error {
		if err := js.shares.UpdateAccessRights(share, AccessRights{AccountID: acc.ID}); err != nil {
			return fmt.Errorf("failed to update access rights: %w", err)
		}
		return nil
	})
}

// ClearAccessRights removes all access rights for the given account.
func (js *JSONStore) ClearAccessRights(acc Account) error {
	return js.update(func(d *jsonData) error {
		removePolicies(d, func(p jsonPolicy) bool { return p.AccountID == acc.ID })
		return nil
	}, func() error {
		js.shares.RemoveAccess(acc)
		return nil
	})
}

// RegisterShare registers a new share in the store.
// Only renterd shares are supported in the Lite mode.
func (js *JSONStore) RegisterShare(s Share) error {
	if s.Type != "renterd" {
		return fmt.Errorf("failed to register share: %s shares are %w", s.Type, ErrLiteMode)
	}
	s.CreatedAt = time.Now()
	return js.update(func(d *jsonData) error {
		for _, sh := range d.Shares {
			if sh.Name == s.Name {
				return fmt.Errorf("failed to register share: share already exists")
			}
		}
		d.Shares = append(d.Shares, s)
		return nil
	}, func() error {
		if err := js.shares.RegisterShare(s); err != nil {
			return fmt.Errorf("failed to add share: %w", err)
		}
		return nil
	})
}

// UnregisterShare removes the share from the store together with
// the associated connections and access policies.
func (js *JSONStore) UnregisterShare(name string) error {
	if name == "" {
		return nil
	}

	s, err := js.GetShare(name)
	if err != nil {
		return err
	} else if s.Name == "" {
		return nil
	}

	if err := js.shares.RemoveShare(s); err != nil {
		return fmt.Errorf("failed to close share: %w", err)
	}

	return js.update(func(d *jsonData) error {
		for i, sh := range d.Shares {
			if sh.Name == name {
				d.Shares = append(d.Shares[:i], d.Shares[i+1:]...)
				break
			}
		}
		conns := d.Connections[:0]
		for _, c := range d.Connections {
			if c.ShareName != name {
				conns = append(conns, c)
			}
		}
		d.Connections = conns
		removePolicies(d, func(p jsonPolicy) bool { return p.ShareName == name })
		return nil
	}, nil)
}

// GetShare tries to retrieve the share information by its name.
func (js *JSONStore) GetShare(name string) (s Share, err error) {
	if name == "" {
		return Share{}, nil
	}
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, sh := range js.data.Shares {
		if sh.Name == name {
			return sh, nil
		}
	}
	return Share{}, nil
}

// GetShares lists all shares the specified account has access to.
func (js *JSONStore) GetShares(acc Account) (shares []Share, err error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	names := make(map[string]struct{})
	for _, p := range js.data.Policies {
		if p.AccountID == acc.ID && (p.ReadAccess || p.WriteAccess || p.DeleteAccess || p.ExecuteAccess) {
			names[p.ShareName] = struct{}{}
		}
	}
	for _, sh := range js.data.Shares {
		if _, ok := names[sh.Name]; ok {
			shares = append(shares, sh)
		}
	}
	return
}

// GetAllShares lists all registered shares.
func (js *JSONStore) GetAllShares() (shares []Share, err error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	shares = append(shares, js.data.Shares...)
	sort.Slice(shares, func(i, j int) bool { return shares[i].Name < shares[j].Name })
	return
}

// GetAccounts lists all the accounts that can connect to the specified share.
func (js *JSONStore) GetAccounts(sh Share) (ars []AccessRights, err error) {
	if sh.Name == "" {
		return nil, nil
	}
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, p := range js.data.Policies {
		if p.ShareName == sh.Name {
			ars = append(ars, accessRightsFromPolicy(p))
		}
	}
	return
}

// AddConnection creates a connection between a workgroup and a share.
func (js *JSONStore) AddConnection(wg Workgroup, share Share, appKey types.PrivateKey) error {
	u := wg.UUID.String()
	return js.update(func(d *jsonData) error {
		if _, ok := findWorkgroupByUUID(d, u); !ok {
			return fmt.Errorf("failed to add connection: workgroup not found")
		}
		var found bool
		for _, sh := range d.Shares {
			if sh.Name == share.Name {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("failed to add connection: share not found")
		}
		for _, c := range d.Connections {
			if c.Workgroup == u && c.ShareName == share.Name {
				return nil
			}
		}
		d.Connections = append(d.Connections, jsonConnection{Workgroup: u, ShareName: share.Name, AppKey: appKey})
		return nil
	}, func() error {
		if err := js.shares.AddConnection(wg, share, appKey); err != nil {
			return fmt.Errorf("failed to connect share: %w", err)
		}
		return nil
	})
}

// RemoveConnection removes the connection between a workgroup and a share
// together with the associated access policies.
func (js *JSONStore) RemoveConnection(wg Workgroup, share Share) error {
	u := wg.UUID.String()
	return js.update(func(d *jsonData) error {
		for i, c := range d.Connections {
			if c.Workgroup == u && c.ShareName == share.Name {
				d.Connections = append(d.Connections[:i], d.Connections[i+1:]...)
				break
			}
		}
		removePolicies(d, func(p jsonPolicy) bool {
			return p.Workgroup == u && p.ShareName == share.Name
		})
		return nil
	}, func() error {
		if err := js.shares.RemoveConnection(wg, share); err != nil {
			return fmt.Errorf("failed to disconnect share: %w", err)
		}
		return nil
	})
}

// IsConnected checks if a connection exists between a workgroup and a share.
func (js *JSONStore) IsConnected(wg Workgroup, share Share) (bool, types.PrivateKey, error) {
	u := wg.UUID.String()
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, c := range js.data.Connections {
		if c.Workgroup == u && c.ShareName == share.Name {
			return true, c.AppKey, nil
		}
	}
	return false, nil, nil
}

// SetAppKey sets the app key for the connection between a workgroup and a share.
func (js *JSONStore) SetAppKey(wg Workgroup, share Share, key types.PrivateKey) error {
	u := wg.UUID.String()
	return js.update(func(d *jsonData) error {
		for i, c := range d.Connections {
			if c.Workgroup == u && c.ShareName == share.Name {
				d.Connections[i].AppKey = key
				return nil
			}
		}
		return nil
	}, nil)
}

// findWorkgroupByUUID looks up a workgroup by its UUID string.
func findWorkgroupByUUID(d *jsonData, u string) (Workgroup, bool) {
	for _, w := range d.Workgroups {
		if w.UUID.String() == u {
			return w, true
		}
	}
	return Workgroup{}, false
}

// removePolicies deletes all policies matching the predicate.
func removePolicies(d *jsonData, match func(jsonPolicy) bool) {
	policies := d.Policies[:0]
	for _, p := range d.Policies {
		if !match(p) {
			policies = append(policies, p)
		}
	}
	d.Policies = policies
}

// accessRightsFromPolicy converts a persisted policy to an AccessRights structure.
func accessRightsFromPolicy(p jsonPolicy) AccessRights {
	return AccessRights{
		ShareName:     p.ShareName,
		AccountID:     p.AccountID,
		ReadAccess:    p.ReadAccess,
		WriteAccess:   p.WriteAccess,
		DeleteAccess:  p.DeleteAccess,
		ExecuteAccess: p.ExecuteAccess,
	}
}
