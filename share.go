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
	serverName      string
	connectSecurity map[string]struct{}
	fileSecurity    map[string]uint32
	shareType       uint8
	remark          string
	maxUses         int
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
	mu      sync.Mutex
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
		serverName:      ss.ServerName,
		shareType:       smb2.SHARE_TYPE_DISK,
		maxUses:         maxShareUses,
		bucket:          ss.Bucket,
		remark:          ss.Remark,
		connectSecurity: make(map[string]struct{}),
		fileSecurity:    make(map[string]uint32),
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

		accs := make(map[int]stores.Account)
		for _, ar := range ars {
			if _, exists := accs[ar.AccountID]; !exists {
				acc, err := s.store.GetAccountByID(ar.AccountID)
				if err != nil {
					return err
				}
				accs[ar.AccountID] = acc
			}
		}

		sh.mu.Lock()
		for _, ar := range ars {
			acc := accs[ar.AccountID]
			sh.connectSecurity[acc.Workgroup+"/"+acc.Username] = struct{}{}
			sh.fileSecurity[acc.Workgroup+"/"+acc.Username] = stores.FlagsFromAccessRights(ar)
		}
		sh.mu.Unlock()
	}

	s.mu.Lock()
	s.shareList[sh.name] = sh
	s.mu.Unlock()

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

	acc, err := s.store.GetAccountByID(ar.AccountID)
	if err != nil {
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

// AddConnection initializes a per-workgroup indexd SDK client and populates
// the security maps for the connecting workgroup's accounts.
func (s *server) AddConnection(wg stores.Workgroup, share stores.Share, appKey types.PrivateKey) error {
	s.mu.Lock()
	sh, found := s.shareList[share.Name]
	s.mu.Unlock()
	if !found {
		if err := s.RegisterShare(share); err != nil {
			return err
		}
		s.mu.Lock()
		sh, found = s.shareList[share.Name]
		s.mu.Unlock()
		if !found {
			return errShareNotFound
		}
	}

	if sh.backend == "indexd" {
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
		c := client.NewIndexdClient(db, sdkClient, share.Name, share.DataShards, share.ParityShards, client.PackingOptions{
			MinSize: s.cfg.MinPackedSlabSize,
			MaxAge:  s.cfg.MaxBufferAge.Duration(),
		})

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

		sh.mu.Lock()
		sh.indexdConns[wg.UUID.String()] = conn
		sh.mu.Unlock()
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
