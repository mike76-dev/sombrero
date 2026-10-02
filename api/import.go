package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/transfer"
	proto "go.sia.tech/core/rhp/v4"
	"go.sia.tech/core/types"
	sdk "go.sia.tech/siastorage"
)

// ImportState is the phase an import is in.
type ImportState string

const (
	// ImportIdle is reported for a workgroup and share with no import tracked.
	ImportIdle ImportState = "idle"

	// ImportRunning is an import in flight, ImportDone one that reached the end
	// of what it was given, and ImportFailed one that could not.
	ImportRunning ImportState = "running"
	ImportDone    ImportState = "done"
	ImportFailed  ImportState = "failed"

	// ImportCancelled is where an import ends that was called off.
	ImportCancelled ImportState = "cancelled"
)

// importRetention is how long a finished import is kept for its outcome to be
// read, and maxImportFailures how many failed paths it holds on to.
const (
	importRetention   = 30 * time.Minute
	maxImportFailures = 20

	// sourceProbeTimeout bounds the look at a source, which is made while
	// whoever asked for it waits.
	sourceProbeTimeout = 30 * time.Second
)

// ImportRequest is the body of POST /import/:workgroup/:share: where the data is
// now, and who it comes to belong to here.
type ImportRequest struct {
	// Source is the kind of server the data is on: "renterd" or "indexd".
	Source  string `json:"source"`
	Address string `json:"address"`

	// Username names the account of the workgroup the imported files belong to.
	Username string `json:"username"`

	// Password and Bucket are what a renterd source is read with, AppKey what an
	// indexd account is read with, hex-encoded.
	Password string `json:"password,omitempty"`
	Bucket   string `json:"bucket,omitempty"`
	AppKey   string `json:"appKey,omitempty"`

	// Prefix is where the objects of an indexd account are put, an account
	// keeping objects rather than the names anything went by.
	Prefix string `json:"prefix,omitempty"`

	// Copy has the data copied even where it could have been pinned, for a
	// source whose slabs are to be left where they are.
	Copy bool `json:"copy,omitempty"`
}

// ImportStatusResponse is the response type of every /import/:workgroup/:share
// call. Path is the file the import is working on, and Failures the first of the
// paths it could not have.
type ImportStatusResponse struct {
	State   ImportState `json:"state"`
	Source  string      `json:"source,omitempty"`
	Started *time.Time  `json:"started,omitempty"`
	Since   *time.Time  `json:"since,omitempty"`

	// Path is the file the import has in hand, with FileBytes of its FileSize
	// moved so far. A file that is pinned rather than copied moves none of it.
	Path      string `json:"path,omitempty"`
	FileBytes uint64 `json:"fileBytes,omitempty"`
	FileSize  uint64 `json:"fileSize,omitempty"`

	Directories int    `json:"directories"`
	Pinned      int    `json:"pinned"`
	Copied      int    `json:"copied"`
	Skipped     int    `json:"skipped"`
	Failed      int    `json:"failed"`
	Bytes       uint64 `json:"bytes"`
	Waits       int    `json:"waits"`

	Failures []string `json:"failures,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// ImportProbeResponse says what a source holds before anything is taken from
// it. For an indexd account, Objects is what it has pinned and Tagged how many
// of the Looked at say which files they are of; Warning is what an import of it
// would come to where they say nothing.
type ImportProbeResponse struct {
	Source  string `json:"source"`
	Objects int    `json:"objects,omitempty"`
	Looked  int    `json:"looked,omitempty"`
	Tagged  int    `json:"tagged,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// Transfers is the part of a store an import needs: the rows of what it pinned,
// and the times an upload stamps over. Only the database-backed store has it.
type Transfers interface {
	client.TransferStore
	SetFileTimes(share, path string, createdAt, modifiedAt time.Time) error
}

// importRun is one import of one source into one workgroup's share. It outlives
// the request that starts it: the work runs in the background, and what follows
// it is GET /import/:workgroup/:share.
type importRun struct {
	source string
	cancel context.CancelFunc

	mu      sync.Mutex
	state   ImportState
	started time.Time
	since   time.Time
	stats   client.ImportStats

	// The file the import has in hand, and how much of it has moved. A large file
	// is one record of the description for a long time, so what it is doing is
	// only visible from inside it.
	path     string
	copied   uint64
	size     uint64
	failures []string
	err      string
}

// working records the file the import has in hand and how much of it has moved.
func (r *importRun) working(path string, copied, total uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.path = path
	r.copied = copied
	r.size = total
}

// count keeps what the import has done so far, which it reports as it goes. It
// comes once a file is done with, so what was moved of it is counted in the
// totals by now and no longer belongs to a file in hand.
func (r *importRun) count(stats client.ImportStats) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stats = stats
	r.path = ""
	r.copied, r.size = 0, 0
}

// failure keeps a path the import could not have, up to the first few of them.
func (r *importRun) failure(path string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.failures) < maxImportFailures {
		r.failures = append(r.failures, fmt.Sprintf("%s: %v", path, err))
	}
}

// finish ends the import with what it came to.
func (r *importRun) finish(state ImportState, stats client.ImportStats, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = state
	r.since = time.Now()
	r.path = ""
	r.copied, r.size = 0, 0
	r.stats = stats
	if err != nil {
		r.err = err.Error()
	}
}

// done reports whether the import has ended.
func (r *importRun) done() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.state != ImportRunning
}

// status reports the import as it stands.
func (r *importRun) status() ImportStatusResponse {
	r.mu.Lock()
	defer r.mu.Unlock()
	started, since := r.started, r.since

	return ImportStatusResponse{
		State:       r.state,
		Source:      r.source,
		Started:     &started,
		Since:       &since,
		Path:        r.path,
		FileBytes:   r.copied,
		FileSize:    r.size,
		Directories: r.stats.Directories,
		Pinned:      r.stats.Pinned,
		Copied:      r.stats.Copied,
		Skipped:     r.stats.Skipped,
		Failed:      r.stats.Failed,

		// What has moved of the file in hand is not in the totals yet, and is
		// counted here so that the bytes do not stand still through a large one.
		Bytes:    r.stats.Bytes + r.copied,
		Waits:    r.stats.Waits,
		Failures: append([]string(nil), r.failures...),
		Error:    r.err,
	}
}

// importTracker keeps the imports, one per workgroup and share.
type importTracker struct {
	mu   sync.Mutex
	runs map[string]*importRun
}

// begin starts an import. A workgroup imports into a share one at a time, so one
// that is still running is returned as it is, with false to say so.
func (t *importTracker) begin(key, source string, cancel context.CancelFunc) (*importRun, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r, running := t.runs[key]; running && !r.done() {
		return r, false
	}

	now := time.Now()
	r := &importRun{source: source, cancel: cancel, state: ImportRunning, started: now, since: now}
	if t.runs == nil {
		t.runs = make(map[string]*importRun)
	}
	t.runs[key] = r

	return r, true
}

// get returns the tracked import, or nil where there is none.
func (t *importTracker) get(key string) *importRun {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.runs[key]
}

// forget drops the import once its outcome has had the time to be read, unless a
// later one has taken its place by then.
func (t *importTracker) forget(ctx context.Context, key string, r *importRun) {
	select {
	case <-time.After(importRetention):
	case <-ctx.Done():
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.runs[key] == r {
		delete(t.runs, key)
	}
}

// importHandlerGET handles the GET /import/:workgroup/:share calls.
// :workgroup may be a UUID or a workgroup name.
func (api *API) importHandlerGET(w http.ResponseWriter, _ *http.Request, ps httprouter.Params) {
	wg, share, ok := api.resolveConnection(w, ps)
	if !ok {
		return
	}

	if run := api.imports.get(connectKey(wg, share)); run != nil {
		writeJSON(w, run.status())
		return
	}

	writeJSON(w, ImportStatusResponse{State: ImportIdle})
}

// importHandlerDELETE handles the DELETE /import/:workgroup/:share calls, which
// call off an import that is still running.
// :workgroup may be a UUID or a workgroup name.
func (api *API) importHandlerDELETE(w http.ResponseWriter, _ *http.Request, ps httprouter.Params) {
	wg, share, ok := api.resolveConnection(w, ps)
	if !ok {
		return
	}

	run := api.imports.get(connectKey(wg, share))
	if run == nil || run.done() {
		writeError(w, "no import is running for this workgroup and share", http.StatusNotFound)
		return
	}

	run.cancel()
	writeJSON(w, run.status())
}

// importProbeHandlerPOST handles the POST /import/:workgroup/:share/probe calls.
// It looks at a source without taking anything from it, so that what an import
// of it would come to is known before one is run.
// :workgroup may be a UUID or a workgroup name.
func (api *API) importProbeHandlerPOST(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	if _, _, ok := api.resolveConnection(w, ps); !ok {
		return
	}

	var body ImportRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.Address == "" {
		writeError(w, "the address of the source cannot be empty", http.StatusBadRequest)
		return
	}

	res := ImportProbeResponse{Source: body.Source}
	switch body.Source {
	case "renterd":
		// A renterd server knows what it calls its objects, so an import of one
		// brings the files over as themselves whatever else is true of it.
		if _, ok := api.renterdSource(w, body); !ok {
			return
		}

	case "indexd":
		account, ok := api.indexdSource(w, body)
		if !ok {
			return
		}

		ctx, cancel := context.WithTimeout(req.Context(), sourceProbeTimeout)
		defer cancel()

		probe, err := client.ProbeAccount(ctx, account)
		if err != nil {
			writeError(w, "the account could not be looked at: "+err.Error(), http.StatusBadGateway)
			return
		}

		res.Objects, res.Looked, res.Tagged = probe.Objects, probe.Looked, probe.Tagged
		switch {
		case probe.Looked > 0 && probe.Tagged == 0:
			res.Warning = fmt.Sprintf("None of the %d objects looked at say which files they hold. They would come over one file per object, each named after the object and holding whatever runs of whichever files were packed into it.", probe.Looked)
		case probe.Tagged < probe.Looked:
			res.Warning = fmt.Sprintf("Only %d of the %d objects looked at say which files they hold. The rest would come over one file per object.", probe.Tagged, probe.Looked)
		}

	default:
		writeError(w, `the source of an import is "renterd" or "indexd"`, http.StatusBadRequest)
		return
	}

	writeJSON(w, res)
}

// importHandlerPOST handles the POST /import/:workgroup/:share calls. It starts
// an import of what another server holds into this share, which then runs on its
// own: the response is the state it starts in, and GET /import/:workgroup/:share
// reports the rest.
// :workgroup may be a UUID or a workgroup name.
func (api *API) importHandlerPOST(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	wg, share, ok := api.resolveConnection(w, ps)
	if !ok {
		return
	}
	if share.Type != "indexd" {
		writeError(w, "only an indexd share takes over what another server holds", http.StatusBadRequest)
		return
	}

	var body ImportRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.Address == "" {
		writeError(w, "the address of the source cannot be empty", http.StatusBadRequest)
		return
	}

	store, ok := api.store.(Transfers)
	if !ok {
		writeError(w, "an import needs the database-backed store", http.StatusBadRequest)
		return
	}

	acc, err := api.store.FindAccount(strings.ToLower(body.Username), wg.UUID.String())
	if err != nil {
		log.Printf("failed to find account: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if acc.ID == 0 {
		writeError(w, "the imported files belong to an account of this workgroup, and there is no such account", http.StatusBadRequest)
		return
	}

	dst, ok := api.destination(w, wg, share)
	if !ok {
		return
	}

	src, _, describe, ok := api.source(w, body, share)
	if !ok {
		return
	}

	// What is taken over is pinned into the account behind the destination's own
	// connection. Pinning it into the account it is read from would leave the
	// rows naming objects this share does not hold.
	pinner, _ := dst.(client.ObjectPinner)

	ctx, cancel := context.WithCancel(api.ctx)
	run, started := api.imports.begin(connectKey(wg, share), body.Source, cancel)
	if !started {
		cancel()
		writeError(w, "this workgroup is already importing into this share", http.StatusConflict)
		return
	}

	opts := client.ImportOptions{
		CopyOptions: client.CopyOptions{
			Account:   acc,
			ChunkSize: uint64(share.DataShards) * proto.SectorSize,
			Stamp: func(_ context.Context, file transfer.File) error {
				return store.SetFileTimes(share.Name, file.Path, file.CreatedAt, file.ModifiedAt)
			},
			Progress: run.working,
			OnError:  run.failure,
		},
		Target: stores.TransferTarget{Share: share.Name, Workgroup: wg.ID, Owner: acc},
		Copy:   body.Copy,
		Report: run.count,
	}

	go api.runImport(ctx, cancel, run, connectKey(wg, share), store, dst, src, pinner, describe, opts)

	writeJSONStatus(w, http.StatusAccepted, run.status())
}

// runImport carries an import through to its end. The source is walked into a
// description as the import reads it, so that nothing has to hold what a whole
// server holds.
func (api *API) runImport(ctx context.Context, cancel context.CancelFunc, run *importRun, key string, store Transfers, dst client.Client, src client.PartReader, pinner client.ObjectPinner, describe describer, opts client.ImportOptions) {
	defer cancel()
	defer func() { go api.imports.forget(api.ctx, key, run) }()

	pr, pw := io.Pipe()
	go func() {
		w, err := transfer.NewWriter(pw, transfer.Header{
			CreatedAt: time.Now(),
			Source:    run.source,
			Origin:    describe.origin,
			Share:     opts.Target.Share,
		})
		if err == nil {
			_, err = describe.walk(ctx, w)
			if err == nil {
				err = w.Close()
			}
		}
		_ = pw.CloseWithError(err)
	}()

	r, err := transfer.NewReader(pr)
	if err != nil {
		run.finish(ImportFailed, client.ImportStats{}, err)
		return
	}

	stats, err := client.Import(ctx, store, dst, src, pinner, r, opts)
	_ = pr.CloseWithError(err)

	switch {
	case errors.Is(err, context.Canceled):
		run.finish(ImportCancelled, stats, nil)
	case err != nil:
		log.Printf("the import into %s failed: %v", opts.Target.Share, err)
		run.finish(ImportFailed, stats, err)
	default:
		run.finish(ImportDone, stats, nil)
	}
}

// describer walks a source into a description, and says where that source is.
type describer struct {
	origin string
	walk   func(ctx context.Context, w *transfer.Writer) (client.DescribeStats, error)
}

// destination returns the client of the workgroup's connection to the share,
// which is what an import writes through.
func (api *API) destination(w http.ResponseWriter, wg stores.Workgroup, share stores.Share) (client.Client, bool) {
	if api.server == nil {
		writeError(w, "the server is not serving this share", http.StatusBadRequest)
		return nil, false
	}

	conns, _, err := api.server.ShareConnections(share.Name)
	if err != nil {
		log.Printf("failed to get the connections of the share: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}

	dst, ok := conns[wg.UUID.String()]
	if !ok {
		writeError(w, "this workgroup is not connected to this share", http.StatusBadRequest)
		return nil, false
	}

	return dst, true
}

// source builds what the data is read from, and the walk that describes it.
func (api *API) source(w http.ResponseWriter, body ImportRequest, share stores.Share) (client.PartReader, client.ObjectPinner, describer, bool) {
	switch body.Source {
	case "renterd":
		rc, ok := api.renterdSource(w, body)
		if !ok {
			return nil, nil, describer{}, false
		}

		// renterd encrypts its shards to a scheme of its own, so there is nothing
		// of it to pin: what it holds is copied.
		return client.RenterdSource{Client: rc}, nil, describer{
			origin: body.Address,
			walk: func(ctx context.Context, tw *transfer.Writer) (client.DescribeStats, error) {
				return rc.Describe(ctx, tw)
			},
		}, true

	case "indexd":
		account, ok := api.indexdSource(w, body)
		if !ok {
			return nil, nil, describer{}, false
		}

		prefix := body.Prefix

		return client.IndexdSource{Objects: account}, nil, describer{
			origin: body.Address,
			walk: func(ctx context.Context, tw *transfer.Writer) (client.DescribeStats, error) {
				return client.DescribeAccount(ctx, account, tw, body.Address, prefix)
			},
		}, true

	default:
		writeError(w, `the source of an import is "renterd" or "indexd"`, http.StatusBadRequest)
		return nil, nil, describer{}, false
	}
}

// renterdSource builds the client a renterd source is read with.
func (api *API) renterdSource(w http.ResponseWriter, body ImportRequest) (*client.RenterdClient, bool) {
	rc, ok := client.NewRenterdClient(body.Address, body.Password, body.Bucket).(*client.RenterdClient)
	if !ok {
		writeError(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}

	return rc, true
}

// indexdSource builds the account an indexd source is read with, which takes the
// app key of that account: it is what its objects are sealed to.
func (api *API) indexdSource(w http.ResponseWriter, body ImportRequest) (*client.IndexdAccount, bool) {
	key, err := hex.DecodeString(body.AppKey)
	if err != nil || len(key) != 64 {
		writeError(w, "an indexd source is read with the 64-byte app key of its account", http.StatusBadRequest)
		return nil, false
	}

	sdkClient, err := sdk.NewBuilder(body.Address, api.appMetadata()).SDK(types.PrivateKey(key))
	if err != nil {
		writeError(w, "the app key is not authorized by that indexer: "+err.Error(), http.StatusBadRequest)
		return nil, false
	}

	return client.NewIndexdAccount(sdkClient), true
}

// appMetadata is how this server names itself to an indexer, which is what its
// app key is derived under.
func (api *API) appMetadata() sdk.AppMetadata {
	return sdk.AppMetadata{
		ID:          types.HashBytes(append([]byte(api.cfg.Indexd.Name), []byte(api.cfg.Indexd.Description)...)),
		Name:        api.cfg.Indexd.Name,
		Description: api.cfg.Indexd.Description,
		LogoURL:     api.cfg.Indexd.LogoURL,
		ServiceURL:  api.cfg.Indexd.ServiceURL,
	}
}
