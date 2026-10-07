// Package transfer describes the files a server keeps on the Sia network in a
// form another server can take over. A description is not the data: a part of a
// file says where its bytes are to be had, not what they are.
package transfer

import (
	"errors"
	"fmt"
	"time"

	"go.sia.tech/core/types"
	"go.sia.tech/indexd/slabs"
)

// Version is the version of the format this package writes. A reader accepts
// what it knows and refuses what it does not.
const Version = 1

// magic opens every stream, so that a file that is not a transfer is refused
// before anything in it is believed.
const magic = "sombrero/transfer"

// maxRecordSize bounds what one record may claim to be, which bounds what a
// reader allocates for it. A file's parts have to fit in one.
const maxRecordSize = 64 << 20

// CatalogFolder is where a share keeps the catalogs of itself, as files like any
// other. It is named here so that whoever tags a slab knows to keep their names.
const CatalogFolder = "/.sombrero/catalog"

// The kinds of record a stream is made of. Kinds a reader does not know are
// skipped, so that a newer writer stays readable.
const (
	kindEnd uint8 = iota
	kindHeader
	kindDirectory
	kindFile
	kindConnection
	kindServer
)

// Header says where a description came from and what it covers. It opens every
// stream, before anything it describes.
type Header struct {
	// CreatedAt is when the description was made, which is what an importer
	// reports and a restore compares against what it finds.
	CreatedAt time.Time

	// Source names the kind of server the data lives on now: "sombrero",
	// "indexd" or "renterd". Origin is the address it was read from.
	Source string
	Origin string

	// Share and Workgroup are what the description was read from, as hints for
	// whoever reads it: neither has to exist on the server that takes it on.
	Share     string
	Workgroup string
}

// Connection is what a catalog carries besides the folders and files: the share,
// the workgroup with its accounts, who may do what on the share, and the app key
// the connection was made with. It comes right after the header, since nothing
// can be placed before the accounts that own it exist.
type Connection struct {
	Share     Share
	Workgroup Workgroup
	Accounts  []Account
	Policies  []Policy

	// AppKey is the key the workgroup's account at the indexer is reached with.
	// Without it the data is unreadable, which is why a catalog on the network
	// cannot be the only copy of this record.
	AppKey []byte
}

// Server is what a catalog of the server itself carries: what belongs to no
// one connection. Every share as registered, every workgroup with its accounts,
// and the hosts that are banned. A stream carrying it holds no folders or files.
type Server struct {
	Shares     []Share
	Workgroups []WorkgroupAccounts
	Bans       []Ban
}

// WorkgroupAccounts is a workgroup with the accounts in it.
type WorkgroupAccounts struct {
	Workgroup Workgroup
	Accounts  []Account
}

// Ban is a host that is turned away, and why.
type Ban struct {
	Host   string
	Reason string
}

// Share is a share as registered: where it is served from and how.
type Share struct {
	Name           string
	Type           string
	Server         string
	Password       string
	Bucket         string
	Remark         string
	CreatedAt      time.Time
	DataShards     uint8
	ParityShards   uint8
	AllowGuest     bool
	AllowAnonymous bool
	PublicDir      string
	SkipBackup     bool
}

// Workgroup is a workgroup by its identity, with the folders it shares.
type Workgroup struct {
	UUID       [16]byte
	Name       string
	PublicDirs []PublicDir
}

// PublicDir is a folder every member of the workgroup sees.
type PublicDir struct {
	Path          string
	ReadOnly      bool
	CaseSensitive bool
}

// Account is an account of the workgroup, with the hash it authenticates by.
type Account struct {
	Name         string
	PasswordHash []byte
	CreatedAt    time.Time
}

// Policy is what one account may do on the share.
type Policy struct {
	Account string
	Read    bool
	Write   bool
	Delete  bool
	Execute bool
}

// Directory is a folder of a share, with the flags that decide who sees it.
// Paths are normalized the way the store keeps them: forward slashes, a leading
// slash, no trailing one. Owner names the account the folder belongs to; empty,
// it belongs to whoever applies the description.
type Directory struct {
	Path       string
	Owner      string
	Private    bool
	ReadOnly   bool
	CreatedAt  time.Time
	ModifiedAt time.Time
}

// File is one file of a share and the parts its contents are made of. The parts
// are ordered and do not overlap, but they need not cover the file: bytes that
// are still buffered at the source are described by no part at all. Owner is as
// for a Directory.
type File struct {
	Path       string
	Owner      string
	Size       uint64
	CreatedAt  time.Time
	ModifiedAt time.Time
	Parts      []Part
}

// Part is a run of bytes of a file, and where they are to be had. Object names
// the object that holds them; Pin, Source and Inline are the three ways of
// getting at them, of which a part carries as many as its writer could.
type Part struct {
	// Offset is where the part begins in the file, DataOffset where its bytes
	// begin in the object, and Length how many of them there are.
	Offset     uint64
	DataOffset uint64
	Length     uint64

	// Object is the key of the object the bytes live in, which is all a server
	// that already holds that object needs. It is zero where there is none.
	Object types.Hash256

	// Pin is what the object is made of, for a server that has to pin it itself.
	Pin *Pin

	// Source is where the bytes can be fetched from, for when pinning is not to
	// be had and they have to be copied instead.
	Source *Source

	// Inline are the bytes themselves, for the residue too small to be worth a
	// slab of its own.
	Inline []byte
}

// Pin is what an object is made of: the key its data is encrypted with, and the
// slabs it is spread over, each naming the sectors and the hosts that hold them.
type Pin struct {
	DataKey [32]byte
	Slabs   []slabs.SlabSlice
}

// Source is where the bytes of a part can be fetched from when they cannot be
// pinned: the kind of server, the address it answers at, and what it calls them.
type Source struct {
	Kind   string
	Origin string
	Bucket string
	Key    string
}

// End returns where the part ends in the file.
func (p Part) End() uint64 {
	return p.Offset + p.Length
}

// Gap is a run of bytes of a file that no part describes, which is what the
// source had not written to the network yet.
type Gap struct {
	Offset uint64
	Length uint64
}

// Gaps returns the runs of the file no part describes, in order. A file with
// none of them can be had in full from what the description carries.
func (f File) Gaps() []Gap {
	var gaps []Gap
	var at uint64
	for _, part := range f.Parts {
		if part.Offset > at {
			gaps = append(gaps, Gap{Offset: at, Length: part.Offset - at})
		}
		at = part.End()
	}
	if at < f.Size {
		gaps = append(gaps, Gap{Offset: at, Length: f.Size - at})
	}

	return gaps
}

// Complete reports whether the parts cover the whole file.
func (f File) Complete() bool {
	return len(f.Gaps()) == 0
}

var (
	// ErrNotATransfer is returned when a stream does not open as one.
	ErrNotATransfer = errors.New("not a transfer")

	// ErrUnsupportedVersion is returned for a stream written by a newer writer.
	ErrUnsupportedVersion = errors.New("unsupported transfer version")

	// ErrTruncated is returned for a stream that ends before it says it does.
	ErrTruncated = errors.New("transfer ends early")

	// ErrCorrupted is returned when a stream does not hash to what it claims.
	ErrCorrupted = errors.New("transfer does not match its digest")
)

// Validate checks that the directory is one a reader can act on.
func (d Directory) Validate() error {
	return validPath(d.Path)
}

// Validate checks that the connection is one a reader can act on: a share by
// name, and keys and hashes of the lengths they have to be.
func (c Connection) Validate() error {
	if c.Share.Name == "" {
		return errors.New("the connection names no share")
	}
	if len(c.AppKey) != 0 && len(c.AppKey) != 64 {
		return fmt.Errorf("the app key is %d byte(s) long, not 64", len(c.AppKey))
	}

	return validAccounts(c.Accounts)
}

// Validate checks that the server record is one a reader can act on.
func (s Server) Validate() error {
	for _, share := range s.Shares {
		if share.Name == "" {
			return errors.New("a share has no name")
		}
	}
	for _, wg := range s.Workgroups {
		if err := validAccounts(wg.Accounts); err != nil {
			return err
		}
	}
	for _, ban := range s.Bans {
		if ban.Host == "" {
			return errors.New("a ban names no host")
		}
	}

	return nil
}

// validAccounts checks that the accounts have names and hashes of the length a
// hash has.
func validAccounts(accounts []Account) error {
	for _, acc := range accounts {
		if acc.Name == "" {
			return errors.New("an account has no name")
		}
		if len(acc.PasswordHash) != 16 {
			return fmt.Errorf("the password hash of %q is %d byte(s) long, not 16", acc.Name, len(acc.PasswordHash))
		}
	}

	return nil
}

// Validate checks that the file is one a reader can act on: a path it can place,
// and parts that are ordered, do not overlap, stay inside the file, and each say
// where their bytes are to be had.
func (f File) Validate() error {
	if err := validPath(f.Path); err != nil {
		return err
	}

	var at uint64
	for i, part := range f.Parts {
		switch {
		case part.Length == 0:
			return fmt.Errorf("part %d of %q is empty", i, f.Path)
		case part.Offset < at:
			return fmt.Errorf("part %d of %q starts at %d, before the end of the one before it", i, f.Path, part.Offset)
		case part.End() > f.Size:
			return fmt.Errorf("part %d of %q ends at %d, past the end of the file at %d", i, f.Path, part.End(), f.Size)
		case part.Inline != nil && uint64(len(part.Inline)) != part.Length:
			return fmt.Errorf("part %d of %q carries %d byte(s) of a run of %d", i, f.Path, len(part.Inline), part.Length)
		case part.Object == (types.Hash256{}) && part.Pin == nil && part.Source == nil && part.Inline == nil:
			return fmt.Errorf("part %d of %q says nothing about where its bytes are", i, f.Path)
		}
		at = part.End()
	}

	return nil
}

// validPath checks the path convention the store keeps: a leading slash, no
// trailing one, and nothing a reader would have to guess at.
func validPath(path string) error {
	switch {
	case path == "":
		return errors.New("the path is empty")
	case path[0] != '/':
		return fmt.Errorf("the path %q does not start at the root of the share", path)
	case len(path) > 1 && path[len(path)-1] == '/':
		return fmt.Errorf("the path %q ends with a slash", path)
	}

	return nil
}
