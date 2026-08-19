package main

import (
	"context"
	"encoding/binary"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/smb2"
	"github.com/mike76-dev/sombrero/stores"
	proto "go.sia.tech/core/rhp/v4"
	"go.sia.tech/core/types"
	sdk "go.sia.tech/siastorage"
	"lukechampine.com/frand"
)

const (
	maxShareUses = 256 // Not sure if this is a sensible number, real-life testing will show
)

var (
	errShareUnavailable = errors.New("share currently unavailable")
	errShareExists      = errors.New("share with the same name already exists")
	errShareNotFound    = errors.New("share not found")
	errShareInUse       = errors.New("share currently in use by one or more clients")
)

// persistedKey names a file that has been created but not yet uploaded: the workgroup whose
// namespace it is in, and its path within the share.
type persistedKey struct {
	workgroup string
	path      string
}

// ensurePersisted makes the table of files that have been created but not yet uploaded, if it is
// not there yet. sh.mu must be held.
func (sh *share) ensurePersisted() {
	if sh.persisted == nil {
		sh.persisted = make(map[persistedKey]*fileState)
	}
}

// mayConnect reports whether the user is allowed on the share at all.
func (sh *share) mayConnect(workgroup, user string) bool {
	sh.mu.Lock()
	defer sh.mu.Unlock()

	_, ok := sh.connectSecurity[workgroup+"/"+user]

	return ok
}

// fileAccess returns the rights the user holds on the files of the share, and whether they hold
// any. Both tables are rewritten whenever the access rights of an account change, so they are only
// ever read behind the lock those writes take.
func (sh *share) fileAccess(workgroup, user string) (uint32, bool) {
	sh.mu.Lock()
	defer sh.mu.Unlock()

	access, ok := sh.fileSecurity[workgroup+"/"+user]

	return access, ok
}

// indexdConn holds the per-workgroup state for an indexd share connection.
type indexdConn struct {
	client        client.Client
	maxUploadSize uint64
	bucket        string
	createdAt     time.Time
	volumeID      uint64
}

// share represents a Share object.
type share struct {
	name            string
	connectSecurity map[string]struct{}
	fileSecurity    map[string]uint32
	shareType       uint8
	remark          string
	currentUses     int
	encryptData     bool
	compressData    bool

	// For renterd shares (single client shared by all workgroups).
	client        client.Client
	maxUploadSize uint64
	bucket        string
	createdAt     time.Time
	volumeID      uint64

	// For indexd shares (one client per workgroup connection).
	indexdConns map[string]*indexdConn // keyed by workgroup UUID string

	backend string
	// A file that has been created but not yet uploaded has no object behind it: the Sia network
	// takes nothing empty. The workgroup is part of the key because a share is one namespace per
	// workgroup: two workgroups may each hold a file of the same name on the same share, and
	// neither may see the other's.
	persisted map[persistedKey]*fileState

	mu sync.Mutex
}

// RegisterShare adds a new share to the SMB server.
func (s *server) RegisterShare(ss stores.Share) error {
	s.mu.Lock()
	_, found := s.shareList[ss.Name]
	s.mu.Unlock()
	if found {
		return errShareExists
	}

	sh := &share{
		name:            ss.Name,
		backend:         ss.Type,
		shareType:       smb2.SHARE_TYPE_DISK,
		bucket:          ss.Bucket,
		remark:          ss.Remark,
		connectSecurity: make(map[string]struct{}),
		fileSecurity:    make(map[string]uint32),
		persisted:       make(map[persistedKey]*fileState),
		encryptData:     s.encryptData,
		compressData:    s.compressionSupported,
	}

	switch sh.backend {
	case "indexd":
		sh.indexdConns = make(map[string]*indexdConn)
		// Clients are initialized per-connection via AddConnection.
	case "renterd":
		sh.client = client.NewRenterdClient(ss.ServerName, ss.Password, ss.Bucket)
		sh.maxUploadSize = proto.SectorSize

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		info, err := sh.client.Info(ctx)
		if err != nil {
			return errShareUnavailable
		}
		vid := make([]byte, 8)
		frand.Read(vid[:])
		sh.bucket = info.Bucket
		sh.createdAt = time.Time(info.CreatedAt)
		sh.volumeID = binary.LittleEndian.Uint64(vid)
	default:
		return errors.New("unsupported share type")
	}

	// For renterd, pre-populate security maps from existing DB access rights.
	// For indexd, security maps are populated when workgroups connect via AddConnection.
	if sh.backend == "renterd" {
		ars, err := s.store.GetAccounts(ss)
		if err != nil {
			return err
		}
		if err := s.loadAccessRights(sh, ars); err != nil {
			return err
		}
	}

	s.mu.Lock()
	s.shareList[sh.name] = sh
	s.mu.Unlock()

	return nil
}

// loadAccessRights fills the security maps of the share from the stored access rights.
//
// A row naming an account that is no longer there is skipped rather than allowed to fail the
// whole share: one dangling row would otherwise take the share offline for everybody. Skipping
// is also the safe direction, in that the principal the row describes ends up with no access at
// all. The foreign key on the accounts table should keep this from arising; it is handled
// because the share going down is far the worse of the two outcomes if it ever does.
func (s *server) loadAccessRights(sh *share, ars []stores.AccessRights) error {
	accs := make(map[int]stores.Account)
	for _, ar := range ars {
		if _, exists := accs[ar.AccountID]; exists {
			continue
		}
		acc, err := s.store.GetAccountByID(ar.AccountID)
		if errors.Is(err, stores.ErrAccountNotFound) {
			log.Printf("Share %s: skipping access rights of account %d, which no longer exists", sh.name, ar.AccountID)
			continue
		} else if err != nil {
			return err
		}
		accs[ar.AccountID] = acc
	}

	sh.mu.Lock()
	defer sh.mu.Unlock()
	for _, ar := range ars {
		acc, known := accs[ar.AccountID]
		if !known {
			continue
		}
		sh.connectSecurity[acc.Workgroup+"/"+acc.Username] = struct{}{}
		sh.fileSecurity[acc.Workgroup+"/"+acc.Username] = stores.FlagsFromAccessRights(ar)
	}

	return nil
}

// RemoveShare removes a share from the SMB server.
func (s *server) RemoveShare(ss stores.Share) error {
	s.mu.Lock()
	sh, found := s.shareList[ss.Name]
	s.mu.Unlock()
	if !found {
		return errShareNotFound
	}

	sh.mu.Lock()
	if sh.currentUses > 0 {
		sh.mu.Unlock()
		return errShareInUse
	}
	sh.mu.Unlock()

	switch sh.backend {
	case "renterd":
		if sh.client != nil {
			if err := sh.client.DeleteAll(s.ctx); err != nil {
				return err
			} else if err := sh.client.Close(); err != nil {
				return err
			}
		}
	case "indexd":
		for _, conn := range sh.indexdConns {
			if err := conn.client.Close(); err != nil {
				log.Printf("close indexd client: %v", err)
			}
		}
	}

	s.mu.Lock()
	delete(s.shareList, ss.Name)
	s.mu.Unlock()

	return nil
}

// UpdateAccessRights updates the access policy to the share for the given account.
func (s *server) UpdateAccessRights(ss stores.Share, ar stores.AccessRights) error {
	s.mu.Lock()
	sh, found := s.shareList[ss.Name]
	s.mu.Unlock()
	if !found { // Share not loaded yet.
		return nil
	}

	// An account that is not there has nothing to be granted and nothing to revoke: there is
	// no name to key the security maps by. Saying so is better than failing the caller, which
	// is updating a row that describes somebody who no longer exists.
	acc, err := s.store.GetAccountByID(ar.AccountID)
	if errors.Is(err, stores.ErrAccountNotFound) {
		log.Printf("Share %s: ignoring access rights of account %d, which no longer exists", sh.name, ar.AccountID)
		return nil
	} else if err != nil {
		return err
	}

	// For indexd, only update security maps for connected workgroups.
	if sh.backend == "indexd" {
		sh.mu.Lock()
		_, hasConn := sh.indexdConns[acc.Workgroup]
		sh.mu.Unlock()
		if !hasConn {
			return nil
		}
	}

	sh.mu.Lock()
	if ar.ReadAccess || ar.WriteAccess || ar.DeleteAccess || ar.ExecuteAccess {
		sh.connectSecurity[acc.Workgroup+"/"+acc.Username] = struct{}{}
		sh.fileSecurity[acc.Workgroup+"/"+acc.Username] = stores.FlagsFromAccessRights(ar)
	} else {
		delete(sh.connectSecurity, acc.Workgroup+"/"+acc.Username)
		delete(sh.fileSecurity, acc.Workgroup+"/"+acc.Username)
	}
	sh.mu.Unlock()

	return nil
}

// RemoveAccess removes the access rights of the given account from all shares.
func (s *server) RemoveAccess(acc stores.Account) {
	s.mu.Lock()
	for _, sh := range s.shareList {
		sh.mu.Lock()
		delete(sh.connectSecurity, acc.Workgroup+"/"+acc.Username)
		delete(sh.fileSecurity, acc.Workgroup+"/"+acc.Username)
		sh.mu.Unlock()
	}
	s.mu.Unlock()
}

// ensureShare returns the server's state for the given share, registering it
// first if nothing has used it yet. A share only enters the server's list when
// it is first reached for — a tree connect, a connection, a slab scan — so a
// share that exists in the store may still be unknown here.
func (s *server) ensureShare(ss stores.Share) (*share, error) {
	s.mu.Lock()
	sh, found := s.shareList[ss.Name]
	s.mu.Unlock()
	if found {
		return sh, nil
	}

	if err := s.RegisterShare(ss); err != nil {
		return nil, err
	}

	s.mu.Lock()
	sh, found = s.shareList[ss.Name]
	s.mu.Unlock()
	if !found {
		return nil, errShareNotFound
	}

	return sh, nil
}

// AddConnection initializes a per-workgroup indexd SDK client and populates
// the security maps for the connecting workgroup's accounts.
func (s *server) AddConnection(wg stores.Workgroup, share stores.Share, appKey types.PrivateKey) error {
	sh, err := s.ensureShare(share)
	if err != nil {
		return err
	}

	// A workgroup gets one client per share and no more: a second one would
	// claim part of the same buffered pieces, leaving neither with enough to
	// fill a slab. Whoever asks for a connection that is already running —
	// a tree connect, a slab scan, a repeated /connect — keeps that one.
	sh.mu.Lock()
	_, running := sh.indexdConns[wg.UUID.String()]
	sh.mu.Unlock()

	if sh.backend == "indexd" && !running {
		db, ok := s.store.(*stores.Database)
		if !ok {
			return errors.New("indexd shares require a database-backed store")
		}
		builder := sdk.NewBuilder(share.ServerName, sdk.AppMetadata{
			ID:          types.HashBytes(append([]byte(s.cfg.Name), []byte(s.cfg.Description)...)),
			Name:        s.cfg.Name,
			Description: s.cfg.Description,
			LogoURL:     s.cfg.LogoURL,
			ServiceURL:  s.cfg.ServiceURL,
		})
		sdkClient, err := builder.SDK(appKey)
		if err != nil {
			return err
		}
		fragLevel, fragInterval := s.cfg.Fragmentation()
		c := client.NewIndexdClient(db, sdkClient, share.Name, wg.ID, share.DataShards, share.ParityShards, client.PackingOptions{
			MinSize: s.cfg.MinPackedSlabSize,
			MaxAge:  s.cfg.MaxBufferAge.Duration(),
		}, client.FragmentationOptions{
			Threshold: fragLevel,
			Interval:  fragInterval,
		}, s.debug)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		info, err := c.Info(ctx)
		if err != nil {
			return errShareUnavailable
		}

		vid := make([]byte, 8)
		frand.Read(vid[:])

		conn := &indexdConn{
			client:        c,
			maxUploadSize: uint64(share.DataShards) * proto.SectorSize,
			bucket:        info.Bucket,
			createdAt:     time.Time(info.CreatedAt),
			volumeID:      binary.LittleEndian.Uint64(vid),
		}

		// Another caller may have connected the same workgroup while this
		// client was being built. Theirs is the one in use, so this one is
		// closed rather than left running beside it.
		sh.mu.Lock()
		_, taken := sh.indexdConns[wg.UUID.String()]
		if !taken {
			sh.indexdConns[wg.UUID.String()] = conn
		}
		sh.mu.Unlock()

		if taken {
			log.Printf("workgroup %s connected to share %s in the meantime, dropping the second client", wg.UUID, share.Name)
			if err := c.Close(); err != nil {
				log.Printf("failed to close the redundant client: %v", err)
			}
		}
	}

	accs, err := s.store.FindAccounts(wg.UUID.String())
	if err != nil {
		return err
	}

	sh.mu.Lock()
	for _, acc := range accs {
		ar, err := s.store.GetAccessRights(share, acc)
		if err != nil {
			sh.mu.Unlock()
			return err
		}
		if ar.AccountID != 0 && (ar.ReadAccess || ar.WriteAccess || ar.DeleteAccess || ar.ExecuteAccess) {
			sh.connectSecurity[acc.Workgroup+"/"+acc.Username] = struct{}{}
			sh.fileSecurity[acc.Workgroup+"/"+acc.Username] = stores.FlagsFromAccessRights(ar)
		}
	}
	sh.mu.Unlock()

	return nil
}

// ShareConnections returns the clients of the share's workgroup connections,
// keyed by workgroup UUID. Only indexd shares have them: a renterd share is
// served by one client that pins nothing of its own, so it is reported as not
// scannable rather than as an empty set.
//
// A connection whose client is not running is started from its stored app key,
// the same way a tree connect does it, so that what is returned is every
// workgroup connected to the share rather than only those that happen to have
// a client right now. failed names the workgroups that could not be started,
// so that a caller counting what it found can say that it did not see all of
// it.
func (s *server) ShareConnections(name string) (map[string]client.Client, map[string]string, error) {
	share, err := s.store.GetShare(name)
	if err != nil {
		return nil, nil, err
	}
	if share.Name == "" {
		return nil, nil, errShareNotFound
	}

	// A share is registered with the server when something first uses it, so
	// on a server nobody has connected to yet there is nothing to look up.
	sh, err := s.ensureShare(share)
	if err != nil {
		return nil, nil, err
	}

	if sh.backend != "indexd" {
		return nil, nil, client.ErrNoSlabScan
	}

	// indexd shares are database-backed, so the connections are there to be
	// restored even when nothing has used them since the server started.
	db, ok := s.store.(*stores.Database)
	if !ok {
		return nil, nil, errors.New("indexd shares require a database-backed store")
	}
	stored, err := db.ShareConnections(name)
	if err != nil {
		return nil, nil, err
	}

	failed := make(map[string]string)
	for _, conn := range stored {
		u := conn.Workgroup.String()

		sh.mu.Lock()
		_, running := sh.indexdConns[u]
		sh.mu.Unlock()
		if running {
			continue
		}

		wg, err := s.store.FindWorkgroup(conn.Workgroup)
		if err != nil || wg.ID == 0 {
			log.Printf("failed to resolve workgroup %s of share %s: %v", u, name, err)
			failed[u] = "workgroup not found"
			continue
		}
		if err := s.AddConnection(wg, share, conn.AppKey); err != nil {
			log.Printf("failed to restore the connection of workgroup %s to share %s: %v", u, name, err)
			failed[u] = err.Error()
		}
	}

	sh.mu.Lock()
	defer sh.mu.Unlock()
	conns := make(map[string]client.Client, len(sh.indexdConns))
	for wg, conn := range sh.indexdConns {
		conns[wg] = conn.client
		delete(failed, wg)
	}

	return conns, failed, nil
}

// RemoveConnection closes the workgroup's indexd client and removes their
// accounts from the share's security maps.
func (s *server) RemoveConnection(wg stores.Workgroup, share stores.Share) error {
	s.mu.Lock()
	sh, found := s.shareList[share.Name]
	s.mu.Unlock()
	if !found {
		return nil
	}

	if sh.backend == "indexd" {
		sh.mu.Lock()
		if conn, ok := sh.indexdConns[wg.UUID.String()]; ok {
			if err := conn.client.Close(); err != nil {
				log.Printf("close indexd client: %v", err)
			}
			delete(sh.indexdConns, wg.UUID.String())
		}
		sh.mu.Unlock()
	}

	// The files the workgroup created and never uploaded go with it: nothing of that workgroup is
	// on the share any more to ask after them.
	sh.mu.Lock()
	for key := range sh.persisted {
		if key.workgroup == wg.UUID.String() {
			delete(sh.persisted, key)
		}
	}
	sh.mu.Unlock()

	accs, err := s.store.FindAccounts(wg.UUID.String())
	if err != nil {
		return err
	}

	sh.mu.Lock()
	for _, acc := range accs {
		delete(sh.connectSecurity, acc.Workgroup+"/"+acc.Username)
		delete(sh.fileSecurity, acc.Workgroup+"/"+acc.Username)
	}
	sh.mu.Unlock()

	return nil
}
