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

// TestLogFragmentationReportsAChangeOnce checks that a reading is logged when
// it differs from the last one, and again after the slabs were all released.
func TestLogFragmentationReportsAChangeOnce(t *testing.T) {
	var out syncBuffer
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	ic := &IndexdClient{share: "s", workgroup: 7, fragLevel: 0.1}
	same := stores.FragmentationStats{Slabs: 1, Wasted: 100, Fragmented: 1, FragmentedWasted: 100}
	for range 3 {
		ic.logFragmentation(same)
	}
	if got := strings.Count(out.String(), "dead space"); got != 1 {
		t.Fatalf("the same reading three times: logged %d time(s)\n%s", got, out.String())
	}

	ic.logFragmentation(stores.FragmentationStats{Slabs: 1, Wasted: 200, Fragmented: 1, FragmentedWasted: 200})
	if got := strings.Count(out.String(), "dead space"); got != 2 {
		t.Fatalf("a changed reading: logged %d time(s) in all\n%s", got, out.String())
	}

	ic.logFragmentation(stores.FragmentationStats{})
	ic.logFragmentation(same)
	if got := strings.Count(out.String(), "dead space"); got != 3 {
		t.Fatalf("the first reading after the slabs were released: logged %d time(s) in all\n%s", got, out.String())
	}
}
