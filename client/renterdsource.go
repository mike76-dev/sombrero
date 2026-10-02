package client

import (
	"context"
	"fmt"
	"strings"

	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/transfer"
)

// DescribeStats is what walking a source came to.
type DescribeStats struct {
	Directories int
	Files       int
}

// Source says where this client keeps its data, which is what a description of
// that data carries for whoever copies it.
func (rc *RenterdClient) Source() transfer.Source {
	return transfer.Source{Kind: "renterd", Origin: rc.baseURL, Bucket: rc.bucket}
}

// Count walks the bucket and counts the files in it, which is what an import of
// it has to get through. It lists and downloads nothing, so it costs a request
// per folder and no data.
func (rc *RenterdClient) Count(ctx context.Context) (int, error) {
	var files int

	queue := []string{"/"}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return files, err
		}

		dir := queue[0]
		queue = queue[1:]

		entries, err := rc.List(ctx, stores.Account{}, dir)
		if err != nil {
			return files, fmt.Errorf("failed to list %q: %w", dir, err)
		}

		for _, entry := range entries {
			path := describedPath(entry.Key)
			if path == "/" {
				continue
			}
			if strings.HasSuffix(entry.Key, "/") {
				queue = append(queue, path+"/")
				continue
			}
			files++
		}
	}

	return files, nil
}

// Describe walks the bucket and writes a description of what it holds.
//
// The parts it writes say where the bytes are to be fetched from, never how to
// pin them: renterd encrypts its shards to a scheme of its own, so its slabs
// cannot be taken over as they are, only copied.
func (rc *RenterdClient) Describe(ctx context.Context, w *transfer.Writer) (stats DescribeStats, err error) {
	source := rc.Source()

	// Folders are walked widest first, so that a description reads in the order
	// it is applied: a folder before what it holds.
	queue := []string{"/"}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return stats, err
		}

		dir := queue[0]
		queue = queue[1:]

		entries, err := rc.List(ctx, stores.Account{}, dir)
		if err != nil {
			return stats, fmt.Errorf("failed to list %q: %w", dir, err)
		}

		for _, entry := range entries {
			path := describedPath(entry.Key)
			if path == "/" {
				continue
			}

			if strings.HasSuffix(entry.Key, "/") {
				// renterd knows nothing of accounts, so what it holds is left to
				// the workgroup that takes it on rather than to one account of it.
				if err := w.Directory(transfer.Directory{
					Path:       path,
					CreatedAt:  entry.CreatedAt,
					ModifiedAt: entry.ModifiedAt,
				}); err != nil {
					return stats, err
				}
				stats.Directories++
				queue = append(queue, path+"/")

				continue
			}

			file := transfer.File{
				Path:       path,
				Size:       entry.Size,
				CreatedAt:  entry.CreatedAt,
				ModifiedAt: entry.ModifiedAt,
			}

			// A file of no bytes is made of no parts. Anything else is one run to
			// be fetched whole, which whoever copies it cuts up as it uploads.
			if entry.Size > 0 {
				file.Parts = []transfer.Part{{
					Offset: 0,
					Length: entry.Size,
					Source: &transfer.Source{
						Kind:   source.Kind,
						Origin: source.Origin,
						Bucket: source.Bucket,
						Key:    path,
					},
				}}
			}

			if err := w.File(file); err != nil {
				return stats, err
			}
			stats.Files++
		}
	}

	return stats, nil
}

// describedPath is the path a key is described by: from the root of the share,
// with the trailing slash a folder is listed with taken off.
func describedPath(key string) string {
	path := strings.ReplaceAll(key, "\\", "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if path != "/" {
		path = strings.TrimSuffix(path, "/")
	}

	return path
}
