package client

import (
	"encoding/json"
	"log"

	"github.com/mike76-dev/sombrero/stores"
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
