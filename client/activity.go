package client

import (
	"log"

	"github.com/mike76-dev/sombrero/stores"
)

// recordActivity notes for the feed what the account did on the share. The
// operation itself has succeeded, so a failure here is only logged.
func recordActivity(db *stores.Database, share string, acc stores.Account, action, path, target string, size uint64) {
	if db == nil {
		return
	}
	a := stores.Activity{Share: share, Workgroup: acc.Workgroup, Username: acc.Username, Action: action, Path: path, Target: target, Size: size}
	if err := db.RecordActivity(a); err != nil {
		log.Printf("failed to record the activity on %s: %v", share, err)
	}
}
