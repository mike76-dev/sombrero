package client

import (
	"log"
	"os"
	"strings"
	"testing"

	"github.com/mike76-dev/sombrero/stores"
)

// TestLogPackedSlabNamesAPlanOnce checks that a retry of the same plan is not
// printed again, while a plan with other pieces in it is.
func TestLogPackedSlabNamesAPlanOnce(t *testing.T) {
	var out syncBuffer
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	ic := &IndexdClient{share: "s", workgroup: 7, slabSize: 100}
	jobs := []stores.UploadJob{
		{ObjectID: 1, MetadataID: 11, BufferID: 21, Data: []byte("aaaa"), DataLength: 4},
		{ObjectID: 2, MetadataID: 12, BufferID: 22, Data: []byte("bb"), DataLength: 2},
	}
	for range 3 {
		ic.logPackedSlab(jobs, 6)
	}
	if got := strings.Count(out.String(), "packing 2 piece(s)"); got != 1 {
		t.Fatalf("the same plan three times: logged %d time(s)\n%s", got, out.String())
	}

	jobs = append(jobs, stores.UploadJob{ObjectID: 3, MetadataID: 13, BufferID: 23, Data: []byte("c"), DataLength: 1})
	ic.logPackedSlab(jobs, 7)
	if got := strings.Count(out.String(), "packing 3 piece(s)"); got != 1 {
		t.Fatalf("a changed plan: logged %d time(s)\n%s", got, out.String())
	}
}
