package client

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/mike76-dev/sombrero/stores"
	rhpv4 "go.sia.tech/core/rhp/v4"
	"go.sia.tech/core/types"
	"go.sia.tech/renterd/v2/api"
	"golang.org/x/crypto/blake2b"
)

// RenterdClient implements a Client for interacting with renterd.
type RenterdClient struct {
	baseURL  string
	password string
	bucket   string
}

// NewRenterdClient returns an initialized RenterdClient.
func NewRenterdClient(addr, password, bucket string) Client {
	return &RenterdClient{
		baseURL:  addr,
		password: password,
		bucket:   bucket,
	}
}

// doRequest does everything needed to send an HTTP request and receive a response.
func (rc *RenterdClient) doRequest(ctx context.Context, method, path string, body interface{}, resp interface{}) error {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = strings.NewReader(string(data))
	}

	req, err := http.NewRequestWithContext(ctx, method, rc.baseURL+path, reqBody)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	if rc.password != "" {
		req.SetBasicAuth("", rc.password)
	}

	r, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()

	if r.StatusCode < 200 || r.StatusCode >= 300 {
		errMsg, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MiB limit

		// The one status the caller has to be able to tell apart is the object that is not
		// there, which is an answer rather than a failure for anything that was going to take
		// the object away regardless. The other backend already says so with this error, and a
		// caller should not have to know which one is underneath it to read the reply.
		if r.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %s (status: %d)", stores.ErrNotFound, string(errMsg), r.StatusCode)
		}

		return fmt.Errorf("HTTP error: %s (status: %d)", string(errMsg), r.StatusCode)
	}

	if resp != nil {
		return json.NewDecoder(r.Body).Decode(resp)
	}

	return nil
}

// Info queries the general information about the share.
func (rc *RenterdClient) Info(ctx context.Context) (GeneralInfo, error) {
	var bucket api.Bucket
	if err := rc.doRequest(ctx, "GET", fmt.Sprintf("/api/bus/bucket/%s", rc.bucket), nil, &bucket); err != nil {
		return GeneralInfo{}, err
	}

	return GeneralInfo{
		Bucket:    bucket.Name,
		CreatedAt: time.Time(bucket.CreatedAt),
	}, nil
}

// Storage queries the information about the underlying storage.
func (rc *RenterdClient) Storage(ctx context.Context) (StorageInfo, error) {
	r, err := rc.redundancy(ctx)
	if err != nil {
		return StorageInfo{}, err
	}
	if r.MinShards == 0 || r.TotalShards == 0 {
		return StorageInfo{}, errors.New("zero shards not allowed")
	}

	rs, err := rc.remainingStorage(ctx)
	if err != nil {
		return StorageInfo{}, err
	}

	us, err := rc.usedStorage(ctx)
	if err != nil {
		return StorageInfo{}, err
	}

	return StorageInfo{
		Type:             "renterd",
		RemainingStorage: rs,
		UsedStorage:      us,
		MinShards:        r.MinShards,
		TotalShards:      r.TotalShards,
	}, nil
}

// redundancy retrieves the renterd redundancy settings.
func (rc *RenterdClient) redundancy(ctx context.Context) (rs api.RedundancySettings, err error) {
	var resp api.UploadSettings
	err = rc.doRequest(ctx, "GET", "/api/bus/settings/upload", nil, &resp)
	if err != nil {
		return
	}
	return resp.Redundancy, nil
}

// contracts retrieves the renterd contracts.
func (rc *RenterdClient) contracts(ctx context.Context) (contracts []api.ContractMetadata, err error) {
	err = rc.doRequest(ctx, "GET", "/api/bus/contracts", nil, &contracts)
	return
}

// host retrieves the information about a particular host by its public key.
func (rc *RenterdClient) host(ctx context.Context, pk types.PublicKey) (h api.Host, err error) {
	err = rc.doRequest(ctx, "GET", fmt.Sprintf("/api/bus/host/%s", pk), nil, &h)
	return
}

// remainingStorage calculates the total remaining storage of all hosts that renterd has active contracts with.
func (rc *RenterdClient) remainingStorage(ctx context.Context) (rs uint64, err error) {
	var contracts []api.ContractMetadata
	contracts, err = rc.contracts(ctx)
	if err != nil {
		return
	}
	for _, contract := range contracts {
		if contract.State != api.ContractStateActive {
			continue
		}
		var h api.Host
		h, err = rc.host(ctx, contract.HostKey)
		if err != nil {
			return
		}
		rs += h.V2Settings.RemainingStorage
	}
	return rs * rhpv4.SectorSize, nil
}

// usedStorage retrieves the "used" storage, meaning the total size of uploaded sectors including redundancy.
func (rc *RenterdClient) usedStorage(ctx context.Context) (us uint64, err error) {
	values := url.Values{}
	values.Set("bucket", rc.bucket)
	var osr api.ObjectsStatsResponse
	err = rc.doRequest(ctx, "GET", "/api/bus/stats/objects?"+values.Encode(), nil, &osr)
	if err != nil {
		return
	}
	return osr.TotalUploadedSize, nil
}

// IsEmpty returns true if the directory contains no objects.
func (rc *RenterdClient) IsEmpty(ctx context.Context, _ stores.Account, path string) (bool, error) {
	values := url.Values{}
	api.ListObjectOptions{
		Bucket:    rc.bucket,
		Delimiter: "/",
		Limit:     1,
	}.Apply(values)
	path = strings.ReplaceAll(path, "\\", "/") // Replace Windows formatting with the unified one
	path = api.ObjectKeyEscape(path)
	path += "?" + values.Encode()
	var res api.ObjectsResponse
	if err := rc.doRequest(ctx, "GET", fmt.Sprintf("/api/bus/objects/%s", path), nil, &res); err != nil {
		return false, err
	}

	return len(res.Objects) == 0, nil
}

// List lists the contents of a directory.
func (rc *RenterdClient) List(ctx context.Context, _ stores.Account, path string) (ois []ObjectInfo, err error) {
	values := url.Values{}
	api.ListObjectOptions{
		Bucket:    rc.bucket,
		Delimiter: "/",
		Limit:     -1,
	}.Apply(values)
	path = strings.ReplaceAll(path, "\\", "/") // Replace Windows formatting with the unified one
	path = api.ObjectKeyEscape(path)
	path += "?" + values.Encode()
	var res api.ObjectsResponse
	err = rc.doRequest(ctx, "GET", fmt.Sprintf("/api/bus/objects/%s", path), nil, &res)
	if err != nil {
		return
	}

	for _, obj := range res.Objects {
		ois = append(ois, ObjectInfo{
			Key:        obj.Key,
			ETag:       obj.ETag,
			CreatedAt:  time.Time(obj.ModTime),
			ModifiedAt: time.Time(obj.ModTime),
			Size:       uint64(obj.Size),
		})
	}

	return
}

// Object retrieves the information about a file or a directory.
func (rc *RenterdClient) Object(ctx context.Context, _ stores.Account, path string) (ObjectInfo, error) {
	path = strings.ReplaceAll(path, "\\", "/") // Replace Windows formatting with the unified one
	if path == "" {                            // The root path: calculate the total size of the objects
		info, err := rc.Info(ctx)
		if err != nil {
			return ObjectInfo{}, err
		}

		ois, err := rc.List(ctx, stores.Account{}, "")
		if err != nil {
			return ObjectInfo{}, err
		}

		var size uint64
		for _, oi := range ois {
			size += oi.Size
		}

		return ObjectInfo{
			Key:        "/",
			CreatedAt:  info.CreatedAt,
			ModifiedAt: info.CreatedAt,
			Size:       size,
		}, nil
	}

	var parentDir string
	i := strings.LastIndex(path, "/")
	if i < 0 {
		parentDir = ""
	} else {
		parentDir = path[:i+1]
	}

	ois, err := rc.List(ctx, stores.Account{}, parentDir)
	if err != nil {
		return ObjectInfo{}, err
	}

	for _, oi := range ois {
		if oi.Key == "/"+path || oi.Key == "/"+path+"/" {
			return oi, nil
		}
	}

	return ObjectInfo{}, api.ErrObjectNotFound
}

// Parents retrieves the information about the directory at path and the one above it.
func (rc *RenterdClient) Parents(ctx context.Context, _ stores.Account, path string) (currentDir, parentDir FileInfo, err error) {
	// A bucket holds no object for a directory that a listing of it would return, so each of the
	// two is described by the entry that the listing one level above it carries.
	var parent, grandParent string
	if i := strings.LastIndex(path, "/"); i >= 0 {
		parent = path[:i]
		if j := strings.LastIndex(parent, "/"); j >= 0 {
			grandParent = parent[:j]
		}
	}

	var info GeneralInfo
	if path == "" || parent == "" {
		info, err = rc.Info(ctx)
		if err != nil {
			return
		}
	}

	if path == "" {
		currentDir = rootFileInfo(info)
	} else if currentDir, err = rc.dirEntry(ctx, parent, path); err != nil {
		return
	}

	if parent == "" {
		parentDir = rootFileInfo(info)
	} else if parentDir, err = rc.dirEntry(ctx, grandParent, parent); err != nil {
		return
	}

	if parentDir.ID64 == currentDir.ID64 {
		parentDir.ID64 = 0
	}

	return
}

// dirEntry describes the directory at path out of the listing of the directory above it, which is
// where the entry for it lives. A directory the listing does not name is left empty.
func (rc *RenterdClient) dirEntry(ctx context.Context, above, path string) (fi FileInfo, err error) {
	if above != "" {
		above += "/"
	}

	ois, err := rc.List(ctx, stores.Account{}, above)
	if err != nil {
		return
	}

	// The keys of a listing carry the leading separator, and a directory is named with a
	// trailing one.
	key := "/" + path + "/"
	for _, oi := range ois {
		if oi.Key != key {
			continue
		}

		etag, _ := hex.DecodeString(oi.ETag)
		if len(etag) >= 8 {
			fi.ID64 = binary.LittleEndian.Uint64(etag[:8])
		}

		fi.CreatedAt = oi.CreatedAt
		fi.ModifiedAt = oi.ModifiedAt
		fi.ID = make([]byte, 16)
		break
	}

	return
}

// rootFileInfo describes the share root, which no listing holds an entry for.
func rootFileInfo(info GeneralInfo) (fi FileInfo) {
	hash := blake2b.Sum256([]byte("/"))
	fi.ID64 = binary.LittleEndian.Uint64(hash[:8])
	fi.CreatedAt = info.CreatedAt
	fi.ModifiedAt = info.CreatedAt
	fi.ID = make([]byte, 16)

	return
}

// Read downloads a file from the Sia network.
func (rc *RenterdClient) Read(ctx context.Context, _ stores.Account, path string, offset, length uint64, buf io.Writer) (err error) {
	if length == 0 {
		return nil
	}

	values := url.Values{}
	values.Set("bucket", rc.bucket)

	// url.PathEscape does the full escape, so we need to convert any escaped forward slashes back.
	path = strings.ReplaceAll(url.PathEscape(path), "%2F", "/")
	path = "/api/worker/object/" + path + "?" + values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%v%v", rc.baseURL, path), nil)
	if err != nil {
		return
	}

	req.Header.Set("Content-Type", "application/json")

	// The range names the last byte wanted rather than the one past it, so a length of nothing
	// counts back past the first byte and asks for a range ending below where it starts. Worked
	// out unsigned that is the largest number there is, which reads as the whole of the object.
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	if rc.password != "" {
		req.SetBasicAuth("", rc.password)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	defer io.Copy(io.Discard, resp.Body)
	defer resp.Body.Close()
	if !(200 <= resp.StatusCode && resp.StatusCode < 300) {
		err, _ := io.ReadAll(resp.Body)
		return errors.New(string(err))
	}

	if resp == nil {
		return nil
	}

	_, err = io.Copy(buf, resp.Body)
	return
}

// StartUpload initiates a multipart upload.
func (rc *RenterdClient) StartUpload(ctx context.Context, _ stores.Account, path string) (uploadID string, err error) {
	if strings.HasSuffix(path, ":Zone.Identifier") { // Don't upload Windows' zone identifier files
		return
	}
	path = strings.ReplaceAll(path, "\\", "/") // Replace Windows formatting with the unified one
	var resp api.MultipartCreateResponse
	if err = rc.doRequest(ctx, "POST", "/api/bus/multipart/create", api.MultipartCreateRequest{
		Bucket: rc.bucket,
		Key:    "/" + path,
	}, &resp); err != nil {
		return
	}
	return resp.UploadID, nil
}

// AbortUpload aborts an initiated multipart upload.
func (rc *RenterdClient) AbortUpload(ctx context.Context, path string, uploadID string) (err error) {
	if strings.HasSuffix(path, ":Zone.Identifier") { // Don't upload Windows' zone identifier files
		return
	}
	path = strings.ReplaceAll(path, "\\", "/") // Replace Windows formatting with the unified one
	err = rc.doRequest(ctx, "POST", "/api/bus/multipart/abort", api.MultipartAbortRequest{
		Bucket:   rc.bucket,
		Key:      "/" + path,
		UploadID: uploadID,
	}, nil)
	return
}

// FinishUpload completes a multipart upload.
func (rc *RenterdClient) FinishUpload(ctx context.Context, path string, uploadID string, parts []api.MultipartCompletedPart) error {
	if strings.HasSuffix(path, ":Zone.Identifier") { // Don't upload Windows' zone identifier files
		return nil
	}
	path = strings.ReplaceAll(path, "\\", "/") // Replace Windows formatting with the unified one

	// The parts may come out of order, but renterd will error back, so we need to sort them.
	slices.SortFunc(parts, func(a, b api.MultipartCompletedPart) int { return a.PartNumber - b.PartNumber })
	var resp api.MultipartCompleteResponse
	return rc.doRequest(ctx, "POST", "/api/bus/multipart/complete", api.MultipartCompleteRequest{
		Bucket:   rc.bucket,
		Key:      "/" + path,
		UploadID: uploadID,
		Parts:    parts,
	}, &resp)
}

// Write uploads the provided chunk of data to the Sia network.
func (rc *RenterdClient) Write(ctx context.Context, r io.Reader, path string, uploadID string, partNumber int, offset, length uint64) (eTag string, err error) {
	if strings.HasSuffix(path, ":Zone.Identifier") { // Don't upload Windows' zone identifier files
		return
	}
	path = strings.ReplaceAll(path, "\\", "/") // Replace Windows formatting with the unified one
	path = api.ObjectKeyEscape(path)
	values := make(url.Values)
	values.Set("bucket", rc.bucket)
	values.Set("uploadid", uploadID)
	values.Set("partnumber", fmt.Sprint(partNumber))

	off := int(offset)
	opts := api.UploadMultipartUploadPartOptions{
		EncryptionOffset: &off,
		ContentLength:    int64(length),
	}
	opts.Apply(values)

	u, err := url.Parse(fmt.Sprintf("%v/api/worker/multipart/%v", rc.baseURL, path))
	if err != nil {
		return
	}
	u.RawQuery = values.Encode()

	req, err := http.NewRequestWithContext(ctx, "PUT", u.String(), r)
	if err != nil {
		return
	}

	req.SetBasicAuth("", rc.password)
	if length != 0 {
		req.ContentLength = int64(length)
	} else if req.ContentLength, err = sizeFromSeeker(r); err != nil {
		return "", fmt.Errorf("failed to get content length from seeker: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}

	defer resp.Body.Close()
	defer io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		lr := io.LimitReader(resp.Body, 1<<20) // 1MiB limit
		errMsg, _ := io.ReadAll(lr)
		return "", fmt.Errorf("HTTP error: %s (status: %d)", string(errMsg), resp.StatusCode)
	}

	return resp.Header.Get("ETag"), nil
}

// Delete deletes a file or a directory.
func (rc *RenterdClient) Delete(ctx context.Context, _ stores.Account, path string, batch bool) (err error) {
	path = strings.ReplaceAll(path, "\\", "/") // Replace Windows formatting with the unified one
	if batch {
		err = rc.doRequest(ctx, "POST", "/api/worker/objects/remove", api.ObjectsRemoveRequest{
			Bucket: rc.bucket,
			Prefix: "/" + path + "/",
		}, nil)
	} else {
		values := make(url.Values)
		values.Set("bucket", rc.bucket)

		// The name goes into the path of a URL, so it is escaped there as it is everywhere
		// else. Sent as it stands, a name carrying a character that means something in a URL is
		// read as that meaning rather than as part of the name: a file called "a#b.txt" asks to
		// delete "a", the rest being a fragment that never leaves this machine, and "a" is
		// somebody else's object.
		path = api.ObjectKeyEscape(path)
		err = rc.doRequest(ctx, "DELETE", "/api/worker/object/"+path+"?"+values.Encode(), nil, nil)
	}
	return
}

// MakeDirectory creates a new directory in the specified path.
func (rc *RenterdClient) MakeDirectory(ctx context.Context, _ stores.Account, path string) error {
	path = strings.ReplaceAll(path, "\\", "/") // Replace Windows formatting with the unified one
	path = api.ObjectKeyEscape(path)
	path += "/"
	values := make(url.Values)
	values.Set("bucket", rc.bucket)
	return rc.doRequest(ctx, "PUT", "/api/worker/object/"+path+"?"+values.Encode(), nil, nil)
}

// Rename renames a file or a directory.
func (rc *RenterdClient) Rename(ctx context.Context, _ stores.Account, oldName, newName string, isDir, force bool) error {
	mode := api.ObjectsRenameModeSingle
	if isDir {
		oldName += "/"
		newName += "/"
		mode = api.ObjectsRenameModeMulti
	}
	return rc.doRequest(ctx, "POST", "/api/bus/objects/rename", api.ObjectsRenameRequest{
		Bucket: rc.bucket,
		Force:  force,
		From:   "/" + oldName,
		To:     "/" + newName,
		Mode:   mode,
	}, nil)
}

// DeleteAll deletes all objects on the share.
func (rc *RenterdClient) DeleteAll(ctx context.Context) error {
	// `renterd` maintains all objects in the internal database, so we don't need to worry about this.
	return nil
}

// OrphanedSlabs is not supported by renterd shares: renterd holds the objects
// in its own database and drops them together with the files, so there is no
// pinning of this server's making to reconcile.
func (rc *RenterdClient) OrphanedSlabs(ctx context.Context, minAge time.Duration) ([]OrphanedSlab, error) {
	return nil, ErrNoSlabScan
}

// UnpinOrphanedSlabs is not supported by renterd shares, for the reason given
// on OrphanedSlabs.
func (rc *RenterdClient) UnpinOrphanedSlabs(ctx context.Context, minAge time.Duration) (UnpinResult, error) {
	return UnpinResult{}, ErrNoSlabScan
}

// Fragmentation is not supported by renterd shares: renterd packs and repacks
// the objects itself, so this server never sees the slabs they sit in.
func (rc *RenterdClient) Fragmentation(ctx context.Context, threshold float64) (FragmentationReport, error) {
	return FragmentationReport{}, ErrNoSlabScan
}

// Close closes the client and releases all resources.
func (rc *RenterdClient) Close() error {
	return nil
}
