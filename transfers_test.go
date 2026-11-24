package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/mike76-dev/sombrero/api"
	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
)

// meteredStub is a client that only knows what it transferred.
type meteredStub struct {
	client.Client
	t client.Transfer
}

func (m meteredStub) Transferred() client.Transfer { return m.t }

// TestTransfersListsEveryConnection verifies that the transfers come per
// share and workgroup, the renterd share's without a workgroup, in a fixed
// order, and that a client that keeps no count is left out.
func TestTransfersListsEveryConnection(t *testing.T) {
	s := newServerState(context.Background(), nil, stores.Config{})
	s.shareList["pub"] = &share{name: "pub", backend: "renterd", client: meteredStub{t: client.Transfer{Uploaded: 1, Downloaded: 2}}}
	s.shareList["docs"] = &share{name: "docs", backend: "indexd", indexdConns: map[string]*indexdConn{
		"b": {client: meteredStub{t: client.Transfer{Uploaded: 30, Downloaded: 40}}},
		"a": {client: meteredStub{t: client.Transfer{Uploaded: 10, Downloaded: 20}}},
	}}
	s.shareList["mute"] = &share{name: "mute", backend: "renterd", client: struct{ client.Client }{}}

	want := []api.TransferStats{
		{Share: "docs", Workgroup: "a", Uploaded: 10, Downloaded: 20},
		{Share: "docs", Workgroup: "b", Uploaded: 30, Downloaded: 40},
		{Share: "pub", Uploaded: 1, Downloaded: 2},
	}
	if got := s.Transfers(); !reflect.DeepEqual(got, want) {
		t.Errorf("want %+v, got %+v", want, got)
	}
}
