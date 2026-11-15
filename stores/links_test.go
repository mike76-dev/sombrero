package stores

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// linkAccount makes an account of the name in a workgroup of its own.
func linkAccount(t *testing.T, db *Database, username string) Account {
	t.Helper()
	u := uuid.New()
	if err := db.AddWorkgroup(Workgroup{UUID: u}); err != nil {
		t.Fatalf("AddWorkgroup: %v", err)
	}
	if err := db.AddAccount(Account{Username: username, Password: "secret123", Workgroup: u.String()}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	acc, err := db.FindAccount(username, u.String())
	if err != nil {
		t.Fatalf("FindAccount: %v", err)
	}
	return acc
}

// TestLinks verifies that a link is made for an account, found by its token
// with that account, listed on its file for the workgroup and nobody else,
// and deleted by the workgroup only.
func TestLinks(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	alice := linkAccount(t, db, "alice")
	carol := linkAccount(t, db, "carol")

	expires := time.Now().Add(time.Hour).Truncate(time.Second)
	l, err := db.CreateLink("docs", "Projects/plan.pdf", alice, expires)
	if err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if len(l.Token) < 40 || l.Path != "/Projects/plan.pdf" || !l.ExpiresAt.Equal(expires) || time.Since(l.CreatedAt) > time.Minute {
		t.Errorf("want a long token on the absolute path, expiring in an hour, made now, got %+v", l)
	}
	forever, err := db.CreateLink("docs", "/Projects/plan.pdf", alice, time.Time{})
	if err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	got, err := db.Link(l.Token)
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if got.Account.ID != alice.ID || got.Account.Username != alice.Username || got.Account.Workgroup != alice.Workgroup || !got.ExpiresAt.Equal(expires) {
		t.Errorf("want the link with alice's account, got %+v", got)
	}
	if got, err := db.Link(forever.Token); err != nil || !got.ExpiresAt.IsZero() {
		t.Errorf("want the lasting link with no expiry, got %+v, %v", got, err)
	}
	if _, err := db.Link("nothing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown token: want ErrNotFound, got %v", err)
	}

	links, err := db.Links("docs", "/Projects/plan.pdf", alice)
	if err != nil || len(links) != 2 {
		t.Fatalf("want alice's two links, got %+v, %v", links, err)
	}
	if links, err := db.Links("docs", "/Projects/plan.pdf", carol); err != nil || len(links) != 0 {
		t.Errorf("want nothing for another workgroup, got %+v, %v", links, err)
	}
	if links, err := db.Links("docs", "/other.pdf", alice); err != nil || len(links) != 0 {
		t.Errorf("want nothing on another file, got %+v, %v", links, err)
	}

	if err := db.DeleteLink(l.Token, carol); !errors.Is(err, ErrNotFound) {
		t.Errorf("another workgroup deleting: want ErrNotFound, got %v", err)
	}
	if err := db.DeleteLink(l.Token, alice); err != nil {
		t.Errorf("DeleteLink: %v", err)
	}
	if _, err := db.Link(l.Token); !errors.Is(err, ErrNotFound) {
		t.Errorf("a deleted link: want ErrNotFound, got %v", err)
	}
	if err := db.DeleteLink(l.Token, alice); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting again: want ErrNotFound, got %v", err)
	}
}

// TestExpireLinks verifies that the links past their time go and the rest stay.
func TestExpireLinks(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	alice := linkAccount(t, db, "alice")

	old, err := db.CreateLink("docs", "/old.pdf", alice, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	live, err := db.CreateLink("docs", "/live.pdf", alice, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	forever, err := db.CreateLink("docs", "/forever.pdf", alice, time.Time{})
	if err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	if n, err := db.ExpireLinks(); err != nil || n != 1 {
		t.Fatalf("ExpireLinks: want one dropped, got %d, %v", n, err)
	}
	if _, err := db.Link(old.Token); !errors.Is(err, ErrNotFound) {
		t.Errorf("the expired link: want ErrNotFound, got %v", err)
	}
	for _, l := range []Link{live, forever} {
		if _, err := db.Link(l.Token); err != nil {
			t.Errorf("the link to %s: want it kept, got %v", l.Path, err)
		}
	}
}
