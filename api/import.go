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
	"sort"
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

	// ImportCounting is an import working out how much there is to bring over,
	// which it does before it moves any of it so that what follows can be
	// measured against something.
	ImportCounting ImportState = "counting"

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

	// Total is how many files the source turned out to hold, counted before any
	// of them were moved, and Done how many of them are behind us.
	Total int `json:"total,omitempty"`
	Done  int `json:"done"`

	// Refused counts the files the indexer would not take over, which were
	// copied instead, and Refusal is what it said about the first of them. They
	// are not failures: a source on another indexer is refused in full.
	Refused int    `json:"refused"`
	Refusal string `json:"refusal,omitempty"`

	Failures []string `json:"failures,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// ImportProbeResponse says what a source holds before anything is taken from
// it. For an indexd account, Objects is what it has pinned and Tagged how many
// of the Looked at say which files they are of; Warning is what an import of it
// would come to where they say nothing.
// More says the account holds at least what was counted: the look stops a few
// pages into the log, since an exact count of a long one is a request per
// hundred objects and whoever asked is waiting.
type ImportProbeResponse struct {
	Source  string `json:"source"`
	Objects int    `json:"objects,omitempty"`
	Looked  int    `json:"looked,omitempty"`
	Tagged  int    `json:"tagged,omitempty"`
	More    bool   `json:"more,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// ImportSortRequest is the body of POST /import/:workgroup/:share/sort: whose
// the recovered files are, where to look, and where to take up from.
type ImportSortRequest struct {
	Username string `json:"username"`
	Prefix   string `json:"prefix,omitempty"`

	// After is the path the round before this one stopped at, and Limit how many
	// files to look inside in this one.
	After string `json:"after,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// ImportSortResponse is what one round of sorting out the lost and found came
// to. Last is where it stopped and More says there is further to go, which the
// caller asks for in a round of its own.
type ImportSortResponse struct {
	Objects   int    `json:"objects"`
	Recovered int    `json:"recovered"`
	Skipped   int    `json:"skipped"`
	Bytes     uint64 `json:"bytes"`
	Leftover  uint64 `json:"leftover"`
	Last      string `json:"last,omitempty"`
	More      bool   `json:"more,omitempty"`
}

// LostAndFound is the part of a client that sorts out the objects whose names are
// gone. Only an indexd connection has one.
type LostAndFound interface {
	SortLostAndFound(ctx context.Context, acc stores.Account, prefix, after string, limit int) (client.SortReport, error)
}

// ImportSummary is one import the server has in hand: which workgroup is
// importing into which share, and how it is getting on. It is what a page that
// does not know where to look goes by.
type ImportSummary struct {
	Workgroup string               `json:"workgroup"`
	Share     string               `json:"share"`
	Status    ImportStatusResponse `json:"status"`
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

	// Where it is running, so that whoever asks what the server has in hand is
	// told where to look for it: a page that was left and come back to knows
	// neither the workgroup nor the share any more.
	workgroup string
	share     string

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
	total    int
	failures []string
	refusal  string
	err      string
}

// counted records how much there turned out to be, and starts the import on it.
func (r *importRun) counted(total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total = total
	r.state = ImportRunning
	r.since = time.Now()
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

// refused keeps what the indexer said about the first file it would not take
// over. The rest say the same thing, and are counted rather than repeated.
func (r *importRun) refused(path string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refusal == "" {
		r.refusal = err.Error()
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

// done reports whether the import has ended, counting what it is to move being
// part of it rather than a thing before it.
func (r *importRun) done() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.state != ImportRunning && r.state != ImportCounting
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
		Bytes: r.stats.Bytes + r.copied,
		Waits: r.stats.Waits,
		Total: r.total,

		// What is behind us is every file the import is done with, however it
		// came to be done with it.
		Done:     r.stats.Pinned + r.stats.Copied + r.stats.Skipped + r.stats.Failed + r.stats.Unresolved,
		Refused:  r.stats.Refused,
		Refusal:  r.refusal,
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
func (t *importTracker) begin(key, source, workgroup, share string, cancel context.CancelFunc) (*importRun, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r, running := t.runs[key]; running && !r.done() {
		return r, false
	}

	now := time.Now()
	r := &importRun{
		source:    source,
		cancel:    cancel,
		workgroup: workgroup,
		share:     share,
		state:     ImportCounting,
		started:   now,
		since:     now,
	}
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

// summaries returns the imports the server has in hand, the newest first: the
// ones running, and the ones whose outcome is still there to be read.
func (t *importTracker) summaries() []ImportSummary {
	t.mu.Lock()
	runs := make([]*importRun, 0, len(t.runs))
	for _, r := range t.runs {
		runs = append(runs, r)
	}
	t.mu.Unlock()

	summaries := make([]ImportSummary, 0, len(runs))
	for _, r := range runs {
		summaries = append(summaries, ImportSummary{
			Workgroup: r.workgroup,
			Share:     r.share,
			Status:    r.status(),
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		left, right := summaries[i].Status.Started, summaries[j].Status.Started
		if left == nil || right == nil {
			return right == nil
		}

		return left.After(*right)
	})

	return summaries
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

// importsHandlerGET handles the GET /imports calls. It says what imports the
// server has in hand, so that one can be followed and called off by whoever did
// not start it, or by the page that started it and has been reloaded since.
func (api *API) importsHandlerGET(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	writeJSON(w, api.imports.summaries())
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
			writeError(w, "the account could not be read: "+err.Error(), http.StatusBadGateway)
			return
		}

		res.Objects, res.Looked, res.Tagged, res.More = probe.Objects, probe.Looked, probe.Tagged, probe.More
		switch {
		case probe.Looked > 0 && probe.Tagged == 0:
			res.Warning = fmt.Sprintf("None of the %d slabs that were checked are labelled with their file names, so the import will create one file per slab. Each will hold whatever files were packed into that slab, and you can sort them out afterwards.", probe.Looked)
		case probe.Tagged < probe.Looked:
			res.Warning = fmt.Sprintf("Only %d of the %d slabs that were checked are labelled with their file names. The rest will arrive as one file per slab.", probe.Tagged, probe.Looked)
		}

	default:
		writeError(w, `the source of an import is "renterd" or "indexd"`, http.StatusBadRequest)
		return
	}

	writeJSON(w, res)
}

// importSortHandlerPOST handles the POST /import/:workgroup/:share/sort calls.
// It looks inside the objects whose names are gone and makes a file of whatever
// it recognizes in them, one round at a time: each object is downloaded whole, so
// the caller comes back for the next round rather than waiting for all of them.
// :workgroup may be a UUID or a workgroup name.
func (api *API) importSortHandlerPOST(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	wg, share, ok := api.resolveConnection(w, ps)
	if !ok {
		return
	}
	if share.Type != "indexd" {
		writeError(w, "only an indexd share stores the slabs this reads", http.StatusBadRequest)
		return
	}

	var body ImportSortRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeError(w, "invalid body", http.StatusBadRequest)
		return
	}

	acc, err := api.store.FindAccount(strings.ToLower(body.Username), wg.UUID.String())
	if err != nil {
		log.Printf("failed to find account: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if acc.ID == 0 {
		writeError(w, "the recovered files need an owner, and this workgroup has no such account", http.StatusBadRequest)
		return
	}

	dst, ok := api.destination(w, wg, share)
	if !ok {
		return
	}
	sorter, ok := dst.(LostAndFound)
	if !ok {
		writeError(w, "this share does not store the slabs this reads", http.StatusBadRequest)
		return
	}

	report, err := sorter.SortLostAndFound(req.Context(), acc, body.Prefix, body.After, body.Limit)
	if err != nil {
		log.Printf("failed to sort out the lost and found of %s: %v", share.Name, err)
		writeError(w, "the slabs could not be read: "+err.Error(), http.StatusBadGateway)
		return
	}

	writeJSON(w, ImportSortResponse{
		Objects:   report.Objects,
		Recovered: report.Recovered,
		Skipped:   report.Skipped,
		Bytes:     report.Bytes,
		Leftover:  report.Leftover,
		Last:      report.Last,
		More:      report.More,
	})
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
		writeError(w, "only an indexd share can import from another server", http.StatusBadRequest)
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
		writeError(w, "importing needs the database-backed store, which the Lite mode does not use", http.StatusBadRequest)
		return
	}

	acc, err := api.store.FindAccount(strings.ToLower(body.Username), wg.UUID.String())
	if err != nil {
		log.Printf("failed to find account: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if acc.ID == 0 {
		writeError(w, "the imported files need an owner, and this workgroup has no such account", http.StatusBadRequest)
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
	run, started := api.imports.begin(connectKey(wg, share), body.Source, wg.UUID.String(), share.Name, cancel)
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
		Target:    stores.TransferTarget{Share: share.Name, Workgroup: wg.ID, Owner: acc},
		Copy:      body.Copy,
		Report:    run.count,
		OnRefusal: run.refused,
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

	// How much there is to bring over is worked out first, so that what follows
	// can be measured against it. It costs the listing of the source and none of
	// its data.
	var total int
	if describe.count != nil {
		counted, err := describe.count(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				run.finish(ImportCancelled, client.ImportStats{}, nil)
				return
			}
			log.Printf("failed to count what is in %s: %v", describe.origin, err)
			run.finish(ImportFailed, client.ImportStats{}, err)

			return
		}
		total = counted
	}
	run.counted(total)

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

// describer walks a source into a description, says where that source is, and
// counts what is in it before any of it is moved.
type describer struct {
	origin string
	walk   func(ctx context.Context, w *transfer.Writer) (client.DescribeStats, error)
	count  func(ctx context.Context) (int, error)
}

// destination returns the client of the workgroup's connection to the share,
// which is what an import writes through.
func (api *API) destination(w http.ResponseWriter, wg stores.Workgroup, share stores.Share) (client.Client, bool) {
	if api.server == nil {
		writeError(w, "this server is not serving that share", http.StatusBadRequest)
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
			count: rc.Count,
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
			count: func(ctx context.Context) (int, error) {
				return client.CountAccount(ctx, account)
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
		writeError(w, "reading an indexd account needs its 64-byte app key, as 128 hex characters", http.StatusBadRequest)
		return nil, false
	}

	sdkClient, err := sdk.NewBuilder(body.Address, api.appMetadata()).SDK(types.PrivateKey(key))
	if err != nil {
		writeError(w, "that indexer did not accept the app key: "+err.Error(), http.StatusBadRequest)
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
