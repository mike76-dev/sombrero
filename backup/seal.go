package backup

import (
	"crypto/rand"
	"errors"
	"fmt"

	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/chacha20poly1305"
)

// A catalog written into a share can be read by every member of the workgroup
// over SMB, and it holds the app key and the password hashes. So it is sealed
// with a key only the server and a rightful recovery hold: one derived from the
// app key, which a recovery has to have to read the account at all.
const sealedMagic = "sombrero/sealed\n"

// ErrNotSealed is returned for bytes that are not a sealed catalog.
var ErrNotSealed = errors.New("not a sealed catalog")

// catalogKey derives the key a connection's catalogs are sealed with.
func catalogKey(appKey []byte) []byte {
	sum := blake2b.Sum256(append([]byte("sombrero/catalog"), appKey...))
	return sum[:]
}

// Seal encrypts a catalog for the share it goes into.
func Seal(appKey, plain []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(catalogKey(appKey))
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, len(sealedMagic)+aead.NonceSize()+len(plain)+aead.Overhead())
	out = append(out, sealedMagic...)
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out = append(out, nonce...)

	return aead.Seal(out, nonce, plain, []byte(sealedMagic)), nil
}

// Open decrypts a sealed catalog, refusing one sealed under another key or
// changed since.
func Open(appKey, sealed []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(catalogKey(appKey))
	if err != nil {
		return nil, err
	}
	if len(sealed) < len(sealedMagic)+aead.NonceSize()+aead.Overhead() || string(sealed[:len(sealedMagic)]) != sealedMagic {
		return nil, ErrNotSealed
	}

	nonce := sealed[len(sealedMagic) : len(sealedMagic)+aead.NonceSize()]
	plain, err := aead.Open(nil, nonce, sealed[len(sealedMagic)+aead.NonceSize():], []byte(sealedMagic))
	if err != nil {
		return nil, fmt.Errorf("the catalog does not open with this key: %w", err)
	}

	return plain, nil
}
