package client

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/mike76-dev/sombrero/stores"
)

// TestIndexdClientCounts verifies that what an indexd client moves through
// its backend is counted: the whole of an upload, and of a download only
// what was asked.
func TestIndexdClientCounts(t *testing.T) {
	ic := &IndexdClient{backend: newFakeBackend(), dataShards: 1, parityShards: 1}

	data := bytes.Repeat([]byte("x"), 1000)
	key, err := ic.upload(context.Background(), bytes.NewReader(data), nil)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	var out bytes.Buffer
	if err := ic.download(context.Background(), key, 100, 300, &out); err != nil {
		t.Fatalf("download: %v", err)
	}
	if got := ic.Transferred(); got != (Transfer{Uploaded: 1000, Downloaded: 300}) {
		t.Errorf("want 1000 up and 300 down, got %+v", got)
	}
}

// TestRenterdClientCounts verifies that a renterd client counts the bytes of
// its reads and of its writes.
func TestRenterdClientCounts(t *testing.T) {
	f := newFakeRenterd(t)
	c := f.client("default")
	f.answerWith(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write(bytes.Repeat([]byte("y"), 250))
		}
	})

	var buf bytes.Buffer
	if err := c.Read(context.Background(), stores.Account{}, "file.txt", 0, 250, &buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	data := bytes.Repeat([]byte("z"), 400)
	if _, err := c.Write(context.Background(), bytes.NewReader(data), "file.txt", "upload-1", 1, 0, uint64(len(data))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := c.Transferred(); got != (Transfer{Uploaded: 400, Downloaded: 250}) {
		t.Errorf("want 400 up and 250 down, got %+v", got)
	}
}
