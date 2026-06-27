package client

import (
	"os"
	"syscall"
	"testing"
)

// TestMain serializes this package's DB access against other test packages
// that share the same sombrero_test PostgreSQL database.
func TestMain(m *testing.M) {
	f, err := os.OpenFile("/tmp/sombrero_test.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		panic(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		panic(err)
	}
	code := m.Run()
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint
	f.Close()
	os.Exit(code)
}
