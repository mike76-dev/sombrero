package client

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/mike76-dev/sombrero/stores"
	"go.sia.tech/core/types"
)

// objectTagVersion is the version of the tag this server writes. A reader takes
// what it knows and leaves the rest for whoever wrote it.
const objectTagVersion = 1

// objectTag is what an object carries about its own contents: the runs of the
// files whose bytes are in it, and where in it they are.
//
// The indexer stores it sealed with the account's app key, so it is of no use to
// anyone else and of every use to whoever holds the account: an account that was
// all an object store becomes one that can say what its objects were files of.
type objectTag struct {
	Version int           `json:"sombrero"`
	Pieces  []objectPiece `json:"pieces"`
}

// objectPiece is one run of a file's bytes inside an object.
type objectPiece struct {
	Share string `json:"share"`
	Path  string `json:"path"`

	// Offset is where the run begins in the file, At where it begins in the
	// object, and Length how long it is.
	Offset uint64 `json:"offset"`
	At     uint64 `json:"at"`
	Length uint64 `json:"length"`

	// Size is what the file measured when the run was written, which is a hint
	// rather than the truth: a file that was still being written measured less
	// then than it does now.
	Size uint64 `json:"size,omitempty"`
}

// tag returns what the object made of these jobs is to say about its contents.
// A lookup that fails costs the object its tag, not the upload: an object that
// says nothing about itself is what every one written before this was.
func (ic *IndexdClient) tag(jobs []stores.UploadJob) json.RawMessage {
	ids := make([]uint64, 0, len(jobs))
	for _, job := range jobs {
		ids = append(ids, job.MetadataID)
	}

	owners, err := ic.db.PieceOwners(ids)
	if err != nil {
		log.Printf("failed to look up the files of %d piece(s): %v", len(ids), err)
		return nil
	}

	return tagPieces(jobs, owners)
}

// tagPieces returns the tag for an object made of the given jobs, laid out in
// the order they were packed into it. The jobs of a file nobody could name are
// left out, which is what a file deleted while its piece waited leaves behind.
func tagPieces(jobs []stores.UploadJob, owners map[uint64]stores.PieceOwner) json.RawMessage {
	tag := objectTag{Version: objectTagVersion}

	var at uint64
	for _, job := range jobs {
		length := uint64(len(job.Data))
		if owner, ok := owners[job.MetadataID]; ok {
			tag.Pieces = append(tag.Pieces, objectPiece{
				Share:  owner.Share,
				Path:   owner.Path,
				Offset: job.ObjOffset,
				At:     at,
				Length: length,
				Size:   owner.Size,
			})
		}
		at += length
	}

	if len(tag.Pieces) == 0 {
		return nil
	}

	// A tag that cannot be encoded is no reason to leave the data unuploaded:
	// the object is then one that says nothing about itself, as every object
	// written before tagging does.
	meta, err := json.Marshal(tag)
	if err != nil {
		return nil
	}

	return meta
}

// tagOfSlab returns what the object of the slab is to say about itself, worked
// out from the files that reference it now. It is nil for a slab no file does,
// which is one on its way to being unpinned rather than retagged.
func (ic *IndexdClient) tagOfSlab(key types.Hash256) (json.RawMessage, bool) {
	runs, err := ic.db.SlabRuns(ic.share, ic.workgroup, key)
	if err != nil {
		log.Printf("failed to look up the runs of slab %s: %v", key, err)
		return nil, false
	}
	if len(runs) == 0 {
		return nil, false
	}

	tag := objectTag{Version: objectTagVersion, Pieces: make([]objectPiece, 0, len(runs))}
	for _, run := range runs {
		tag.Pieces = append(tag.Pieces, objectPiece{
			Share:  run.Share,
			Path:   run.Path,
			Offset: run.ObjOffset,
			At:     run.DataOffset,
			Length: run.DataLength,
			Size:   run.Size,
		})
	}

	meta, err := json.Marshal(tag)
	if err != nil {
		log.Printf("failed to encode the tag of slab %s: %v", key, err)
		return nil, false
	}

	return meta, true
}

// retag has the objects of these slabs say what they hold now. It is for the
// changes that leave the data where it is and the names of it elsewhere: a file
// that was renamed or deleted out of a slab it shares with others.
//
// The work is handed to a worker, since a client waiting on a rename is not to
// wait on the indexer as well. A tag that is not written is a tag that is out of
// date, which costs a rescue the right name and nothing else.
func (ic *IndexdClient) retag(keys []types.Hash256) {
	if len(keys) == 0 {
		return
	}

	ic.mu.Lock()
	if ic.retagging == nil {
		ic.retagging = make(map[types.Hash256]struct{})
	}
	for _, key := range keys {
		ic.retagging[key] = struct{}{}
	}
	ic.mu.Unlock()

	select {
	case ic.retagChan <- struct{}{}:
	default:
	}
}

// retagObjects is the worker that writes the tags the changes left owing. A tag
// is not worth holding a shutdown up for: what is left unwritten is a name a
// rescue would have got right, and the data is not waiting on it.
func (ic *IndexdClient) retagObjects(ctx context.Context) {
	var retry <-chan time.Time
	for {
		select {
		case <-ic.drainChan:
			return
		case <-ctx.Done():
			return
		case <-ic.retagChan:
		case <-retry:
		}

		// A backend that would not take the tags is waited out rather than
		// taken for an answer: the slabs stay on the list until it does.
		retry = nil
		if ic.writeTags(ctx) {
			retry = time.After(retryDelay)
		}
	}
}

// writeTags tells the objects of the waiting slabs what they hold, and reports
// whether any of them is still owed a tag.
func (ic *IndexdClient) writeTags(ctx context.Context) (owing bool) {
	for _, key := range ic.takeRetags() {
		select {
		case <-ic.drainChan:
			return false
		default:
		}

		if err := ctx.Err(); err != nil {
			return false
		}

		meta, owed := ic.tagOfSlab(key)
		if !owed {
			continue
		}
		if err := ic.backend.Retag(ctx, key, meta); err != nil {
			log.Printf("failed to tell slab %s what it holds, leaving it for a retry: %v", key, err)
			ic.retagLater(key)
			owing = true
		}
	}

	return owing
}

// takeRetags takes the slabs that are waiting off the list, so that one that is
// put back is tried in the next round rather than again in this one.
func (ic *IndexdClient) takeRetags() []types.Hash256 {
	ic.mu.Lock()
	defer ic.mu.Unlock()

	keys := make([]types.Hash256, 0, len(ic.retagging))
	for key := range ic.retagging {
		keys = append(keys, key)
	}
	ic.retagging = nil

	return keys
}

// retagLater puts a slab back on the list, for the round after this one.
func (ic *IndexdClient) retagLater(key types.Hash256) {
	ic.mu.Lock()
	defer ic.mu.Unlock()

	if ic.retagging == nil {
		ic.retagging = make(map[types.Hash256]struct{})
	}
	ic.retagging[key] = struct{}{}
}

// parseTag reads what an object says about its contents, and reports whether it
// said anything this server understands.
func parseTag(meta json.RawMessage) (objectTag, bool) {
	if len(meta) == 0 {
		return objectTag{}, false
	}

	var tag objectTag
	if err := json.Unmarshal(meta, &tag); err != nil {
		return objectTag{}, false
	}
	if tag.Version == 0 || tag.Version > objectTagVersion || len(tag.Pieces) == 0 {
		return objectTag{}, false
	}

	return tag, true
}
