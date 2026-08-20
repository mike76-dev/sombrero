package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mike76-dev/sombrero/stores"
	"go.sia.tech/renterd/v2/api"
)

// recorded is one request as the far end received it, which is the only thing that matters about
// a client: what actually went out on the wire.
type recorded struct {
	path   string // the path of the URL, already unescaped by the server
	rawreq string // the path and query exactly as they arrived
	query  map[string][]string
	body   string
	auth   string
}

// fakeRenterd stands in for renterd. It records every request and answers each with whatever the
// handler for that path was given, so what the client sends can be looked at directly.
type fakeRenterd struct {
	*httptest.Server

	mu      sync.Mutex
	seen    []recorded
	answers map[string]any // by path prefix
	status  map[string]int // by path prefix

	// raw answers the request itself when it is set, for the cases that care about what comes
	// back rather than about what went out. It is read under the lock because the server
	// answers on a goroutine of its own.
	raw http.HandlerFunc
}

func newFakeRenterd(t *testing.T) *fakeRenterd {
	t.Helper()

	f := &fakeRenterd{
		answers: make(map[string]any),
		status:  make(map[string]int),
	}

	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		f.mu.Lock()
		f.seen = append(f.seen, recorded{
			path:   r.URL.Path,
			rawreq: r.URL.RequestURI(),
			query:  r.URL.Query(),
			body:   string(body),
			auth:   r.Header.Get("Authorization"),
		})

		raw := f.raw

		var answer any
		status := http.StatusOK
		for prefix, a := range f.answers {
			if strings.HasPrefix(r.URL.Path, prefix) {
				answer = a
			}
		}
		for prefix, s := range f.status {
			if strings.HasPrefix(r.URL.Path, prefix) {
				status = s
			}
		}
		f.mu.Unlock()

		if raw != nil {
			raw(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if answer != nil {
			_ = json.NewEncoder(w).Encode(answer)
		} else if status == http.StatusOK {
			_, _ = w.Write([]byte("{}"))
		}
	}))

	t.Cleanup(f.Close)

	return f
}

// answerWith hands the request to f itself, for the cases that look at what comes back.
func (f *fakeRenterd) answerWith(h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raw = h
}

// answer sets what comes back for requests whose path starts with prefix.
func (f *fakeRenterd) answer(prefix string, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[prefix] = v
}

// fail makes requests under prefix come back with the given status.
func (f *fakeRenterd) fail(prefix string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[prefix] = status
}

// requests hands back everything that arrived.
func (f *fakeRenterd) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.seen...)
}

// last is the request that arrived most recently.
func (f *fakeRenterd) last(t *testing.T) recorded {
	t.Helper()

	got := f.requests()
	if len(got) == 0 {
		t.Fatal("nothing was sent at all")
	}

	return got[len(got)-1]
}

// client builds a RenterdClient pointed at the stand-in.
func (f *fakeRenterd) client(bucket string) *RenterdClient {
	return &RenterdClient{baseURL: f.URL, password: "the password", bucket: bucket}
}

// TestRenterdDeleteEscapesTheName is a name carrying a character that means something in a URL.
// This was the one method of all of them that put the name into a path without escaping it, and
// the characters that go wrong here are ones an ordinary file may be called: a hash begins a
// fragment, which never leaves this machine, so what reaches the far end is a request to delete
// whatever came before it. That is somebody else's object, and it is deleted.
func TestRenterdDeleteEscapesTheName(t *testing.T) {
	for _, tt := range []struct {
		name string
		path string
	}{
		{"a hash, which begins a fragment", "a#b.txt"},
		{"a question mark, which begins a query", "a?b.txt"},
		{"a space", "a b.txt"},
		{"something already looking escaped", "a%2Fb.txt"},
		{"an ampersand", "a&b.txt"},
		{"a plain name in a directory", "dir/file.txt"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRenterd(t)
			c := f.client("default")

			if err := c.Delete(context.Background(), stores.Account{}, tt.path, false); err != nil {
				t.Fatalf("the delete would not go out: %v", err)
			}

			got := f.last(t)

			// What the far end unescapes out of the path has to be the name that was asked
			// for, and nothing of the name may have leaked into the query.
			if want := "/api/worker/object/" + tt.path; got.path != want {
				t.Errorf("the far end was asked to delete %q, want %q", got.path, want)
			}
			if b := got.query["bucket"]; len(b) != 1 || b[0] != "default" {
				t.Errorf("the bucket arrived as %q, want just the one", b)
			}
		})
	}
}

// TestRenterdMakeDirectoryEscapesTheName is the same question of the method that creates one.
func TestRenterdMakeDirectoryEscapesTheName(t *testing.T) {
	for _, path := range []string{"a#b", "a?b", "a b", "dir/sub"} {
		f := newFakeRenterd(t)
		c := f.client("default")

		if err := c.MakeDirectory(context.Background(), stores.Account{}, path); err != nil {
			t.Fatalf("the request would not go out: %v", err)
		}

		got := f.last(t)
		if want := "/api/worker/object/" + path + "/"; got.path != want {
			t.Errorf("the far end was asked for %q, want %q", got.path, want)
		}
	}
}

// TestRenterdBucketNameSurvivesTheURL is the bucket the server was configured with. The query was
// put together and then handed to a formatter as the thing to format, and what URL encoding
// produces is full of percent signs, which a formatter reads as places to substitute. A bucket
// named anything but plain letters came out as complaints from the formatter in the middle of the
// query, and every request went to the wrong bucket or to none.
func TestRenterdBucketNameSurvivesTheURL(t *testing.T) {
	for _, bucket := range []string{
		"default",
		"my files", // a space, which encodes to a plus
		"a+b",      // a plus, which encodes to a percent sequence
		"100%",     // a percent, which the formatter reads as a verb with nothing behind it
		"naïve",    // outside ASCII, so several percent sequences
		"a&b",      // an ampersand, which separates parameters
		"bucket=x", // something shaped like a parameter of its own
	} {
		t.Run(bucket, func(t *testing.T) {
			f := newFakeRenterd(t)
			c := f.client(bucket)

			// Each of the three methods that built a URL this way.
			if err := c.Delete(context.Background(), stores.Account{}, "file.txt", false); err != nil {
				t.Fatalf("the delete would not go out: %v", err)
			}
			if got := f.last(t).query["bucket"]; len(got) != 1 || got[0] != bucket {
				t.Errorf("deleting reached the bucket %q, want %q", got, bucket)
			}

			if err := c.MakeDirectory(context.Background(), stores.Account{}, "dir"); err != nil {
				t.Fatalf("the request would not go out: %v", err)
			}
			if got := f.last(t).query["bucket"]; len(got) != 1 || got[0] != bucket {
				t.Errorf("making a directory reached the bucket %q, want %q", got, bucket)
			}

			var buf bytes.Buffer
			if err := c.Read(context.Background(), stores.Account{}, "file.txt", 0, 16, &buf); err != nil {
				t.Fatalf("the read would not go out: %v", err)
			}
			if got := f.last(t).query["bucket"]; len(got) != 1 || got[0] != bucket {
				t.Errorf("reading reached the bucket %q, want %q", got, bucket)
			}
		})
	}
}

// TestRenterdReadAsksForTheRangeItWants is the header that says which part of the object is
// wanted. It names the last byte rather than the one after it, so the count has to be taken off
// with that in mind.
func TestRenterdReadAsksForTheRangeItWants(t *testing.T) {
	f := newFakeRenterd(t)
	c := f.client("default")

	var mu sync.Mutex
	var ranges []string
	f.answerWith(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		_, _ = w.Write([]byte("some bytes of the object"))
	})

	for _, tt := range []struct {
		offset, length uint64
		want           string
	}{
		{0, 1, "bytes=0-0"},
		{0, 16, "bytes=0-15"},
		{100, 50, "bytes=100-149"},
	} {
		var buf bytes.Buffer
		if err := c.Read(context.Background(), stores.Account{}, "file.txt", tt.offset, tt.length, &buf); err != nil {
			t.Fatalf("the read would not go out: %v", err)
		}
		mu.Lock()
		got := ranges[len(ranges)-1]
		mu.Unlock()
		if got != tt.want {
			t.Errorf("the range asked for is %q, want %q", got, tt.want)
		}
	}
}

// TestRenterdReadOfNothingAsksForNothing is a read of no length. The range names the last byte
// wanted, so taking one off a count of nothing counts back past the first byte; worked out
// unsigned that is the largest number there is, and the header then asks for the whole object
// however large it is.
func TestRenterdReadOfNothingAsksForNothing(t *testing.T) {
	f := newFakeRenterd(t)
	c := f.client("default")

	var buf bytes.Buffer
	if err := c.Read(context.Background(), stores.Account{}, "file.txt", 0, 0, &buf); err != nil {
		t.Fatalf("a read of nothing was answered with %v", err)
	}

	if buf.Len() != 0 {
		t.Errorf("a read of nothing produced %d bytes", buf.Len())
	}
	for _, r := range f.requests() {
		if got := r.rawreq; got != "" {
			t.Errorf("a read of nothing still asked the far end for %q", got)
		}
	}
}

// TestRenterdReadPassesTheBytesBack is the ordinary case, and the control for the two above.
func TestRenterdReadPassesTheBytesBack(t *testing.T) {
	f := newFakeRenterd(t)
	c := f.client("default")

	want := []byte("the contents of the object")
	f.answerWith(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(want)
	})

	var buf bytes.Buffer
	if err := c.Read(context.Background(), stores.Account{}, "file.txt", 0, uint64(len(want)), &buf); err != nil {
		t.Fatalf("the read would not go out: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("what came back is %q, want %q", buf.Bytes(), want)
	}
}

// TestRenterdSendsItsPassword is what stands between the share and anybody else who can reach the
// far end. Every request carries it.
func TestRenterdSendsItsPassword(t *testing.T) {
	f := newFakeRenterd(t)
	c := f.client("default")

	if _, err := c.Info(context.Background()); err != nil {
		t.Fatalf("the request would not go out: %v", err)
	}

	got := f.last(t)
	if got.auth == "" {
		t.Fatal("the request went out with nothing to identify it")
	}
	if !strings.HasPrefix(got.auth, "Basic ") {
		t.Errorf("the request identified itself with %q", got.auth)
	}
}

// TestRenterdReportsWhatTheFarEndSaid is the request that was refused. What comes back has to be
// an error rather than an empty answer read as a real one: a listing that came back empty because
// the far end was down would have the share look as though everything on it had been deleted.
func TestRenterdReportsWhatTheFarEndSaid(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
	} {
		f := newFakeRenterd(t)
		f.fail("/", status)
		c := f.client("default")

		if _, err := c.Info(context.Background()); err == nil {
			t.Errorf("a status of %d was read as a good answer", status)
		}
		if _, err := c.List(context.Background(), stores.Account{}, "dir"); err == nil {
			t.Errorf("a status of %d was read as an empty directory", status)
		}
		if _, err := c.IsEmpty(context.Background(), stores.Account{}, "dir"); err == nil {
			t.Errorf("a status of %d was read as an answer about a directory", status)
		}
		if err := c.Delete(context.Background(), stores.Account{}, "file.txt", false); err == nil {
			t.Errorf("a status of %d was read as a delete that worked", status)
		}
	}
}

// TestRenterdSaysWhenTheObjectIsNotThere is the one status the caller above has to be able to act
// on. A deletion of a file that is not in the store has already done everything there was to do,
// and the share treats it as such - but only if it can tell that answer apart from the rest, which
// means the error has to carry the sentinel the other backend uses rather than only a status code
// buried in a string.
func TestRenterdSaysWhenTheObjectIsNotThere(t *testing.T) {
	f := newFakeRenterd(t)
	f.fail("/", http.StatusNotFound)
	c := f.client("default")

	err := c.Delete(context.Background(), stores.Account{}, "gone.txt", false)
	if err == nil {
		t.Fatal("a delete of something that is not there came back as a delete that worked")
	}
	if !errors.Is(err, stores.ErrNotFound) {
		t.Errorf("the delete failed with %v, want an error the caller can read as the object not being there", err)
	}
}

// TestRenterdKeepsOtherFailuresApart holds the line above to the one status it is about: anything
// else the far end refuses with is still a failure of its own, and reading it as a missing object
// would have the share above quietly carry on past it.
func TestRenterdKeepsOtherFailuresApart(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	} {
		f := newFakeRenterd(t)
		f.fail("/", status)
		c := f.client("default")

		err := c.Delete(context.Background(), stores.Account{}, "file.txt", false)
		if err == nil {
			t.Errorf("a status of %d was read as a delete that worked", status)
			continue
		}
		if errors.Is(err, stores.ErrNotFound) {
			t.Errorf("a status of %d was read as the object not being there", status)
		}
	}
}

// TestRenterdListReadsWhatCameBack maps the answer of the far end onto what the share above works
// in. A size or a time dropped here is one the client is then told about the file.
func TestRenterdListReadsWhatCameBack(t *testing.T) {
	f := newFakeRenterd(t)
	f.answer("/api/bus/objects", api.ObjectsResponse{
		Objects: []api.ObjectMetadata{
			{Key: "/dir/one.txt", Size: 111, ETag: "aabb"},
			{Key: "/dir/two.txt", Size: 222, ETag: "ccdd"},
			{Key: "/dir/sub/", Size: 0},
		},
	})

	c := f.client("default")
	got, err := c.List(context.Background(), stores.Account{}, "dir")
	if err != nil {
		t.Fatalf("the listing would not come back: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("the listing came back with %d entries, want 3", len(got))
	}
	if got[0].Key != "/dir/one.txt" || got[0].Size != 111 || got[0].ETag != "aabb" {
		t.Errorf("the first entry came back as %+v", got[0])
	}
	if got[2].Key != "/dir/sub/" {
		t.Errorf("the directory came back as %+v", got[2])
	}
}

// TestRenterdIsEmptyReadsTheAnswerRoundTheRightWay is the question asked before a directory is
// deleted. Getting it backwards would have a directory with things in it deleted, or one with
// nothing in it kept for ever.
func TestRenterdIsEmptyReadsTheAnswerRoundTheRightWay(t *testing.T) {
	t.Run("a directory with something in it", func(t *testing.T) {
		f := newFakeRenterd(t)
		f.answer("/api/bus/objects", api.ObjectsResponse{
			Objects: []api.ObjectMetadata{{Key: "/dir/one.txt", Size: 111}},
		})

		empty, err := f.client("default").IsEmpty(context.Background(), stores.Account{}, "dir")
		if err != nil {
			t.Fatalf("the question would not come back: %v", err)
		}
		if empty {
			t.Error("a directory with something in it was called empty")
		}
	})

	t.Run("a directory with nothing in it", func(t *testing.T) {
		f := newFakeRenterd(t)
		f.answer("/api/bus/objects", api.ObjectsResponse{})

		empty, err := f.client("default").IsEmpty(context.Background(), stores.Account{}, "dir")
		if err != nil {
			t.Fatalf("the question would not come back: %v", err)
		}
		if !empty {
			t.Error("a directory with nothing in it was not called empty")
		}
	})
}

// TestRenterdTurnsWindowsSeparatorsRound is the name as it arrives from a client, which separates
// with the other slash. What is stored has to be under the one separator throughout, or a file
// written under one name is looked for later under another.
func TestRenterdTurnsWindowsSeparatorsRound(t *testing.T) {
	f := newFakeRenterd(t)
	c := f.client("default")

	if _, err := c.List(context.Background(), stores.Account{}, "dir\\sub"); err != nil {
		t.Fatalf("the listing would not go out: %v", err)
	}
	if got := f.last(t).path; strings.Contains(got, "\\") {
		t.Errorf("the request went out for %q, which still separates the other way", got)
	}

	if err := c.MakeDirectory(context.Background(), stores.Account{}, "dir\\sub"); err != nil {
		t.Fatalf("the request would not go out: %v", err)
	}
	if got := f.last(t).path; strings.Contains(got, "\\") {
		t.Errorf("the request went out for %q, which still separates the other way", got)
	}
}

// TestRenterdDeleteBatchAsksForThePrefix is what removing a whole directory does: one request
// naming everything under it, rather than one request per object.
func TestRenterdDeleteBatchAsksForThePrefix(t *testing.T) {
	f := newFakeRenterd(t)
	c := f.client("default")

	if err := c.Delete(context.Background(), stores.Account{}, "dir/sub", true); err != nil {
		t.Fatalf("the delete would not go out: %v", err)
	}

	got := f.last(t)
	if got.path != "/api/worker/objects/remove" {
		t.Fatalf("the request went to %q", got.path)
	}

	var req api.ObjectsRemoveRequest
	if err := json.Unmarshal([]byte(got.body), &req); err != nil {
		t.Fatalf("what went out does not read back: %v", err)
	}
	if req.Prefix != "/dir/sub/" {
		t.Errorf("the prefix went out as %q, want %q", req.Prefix, "/dir/sub/")
	}
	if req.Bucket != "default" {
		t.Errorf("the bucket went out as %q", req.Bucket)
	}
}

// TestRenterdDoesNotUploadZoneIdentifiers is the file Windows writes beside a download to say
// where it came from. It is of no use on the share and the three methods of an upload all agree
// to leave it alone, so nothing at all should go out for one.
func TestRenterdDoesNotUploadZoneIdentifiers(t *testing.T) {
	f := newFakeRenterd(t)
	c := f.client("default")

	const path = "download.exe:Zone.Identifier"

	id, err := c.StartUpload(context.Background(), stores.Account{}, path)
	if err != nil {
		t.Fatalf("starting the upload was answered with %v", err)
	}
	if id != "" {
		t.Errorf("an upload was started for a zone identifier: %q", id)
	}

	if err := c.AbortUpload(context.Background(), path, "some-id"); err != nil {
		t.Errorf("aborting was answered with %v", err)
	}
	if err := c.FinishUpload(context.Background(), path, "some-id", nil); err != nil {
		t.Errorf("finishing was answered with %v", err)
	}
	if _, err := c.Write(context.Background(), strings.NewReader("data"), path, "some-id", 1, 0, 4); err != nil {
		t.Errorf("writing was answered with %v", err)
	}

	if got := f.requests(); len(got) != 0 {
		t.Errorf("%d requests went out for a zone identifier", len(got))
	}
}

// TestRenterdFinishUploadSortsTheParts is what the far end insists on. The parts are finished in
// whatever order they happened to complete, and one out of order is refused there rather than
// reordered, so they are put in order before they go.
func TestRenterdFinishUploadSortsTheParts(t *testing.T) {
	f := newFakeRenterd(t)
	c := f.client("default")

	parts := []api.MultipartCompletedPart{
		{PartNumber: 3, ETag: "c"},
		{PartNumber: 1, ETag: "a"},
		{PartNumber: 2, ETag: "b"},
	}

	if err := c.FinishUpload(context.Background(), "file.txt", "some-id", parts); err != nil {
		t.Fatalf("finishing would not go out: %v", err)
	}

	var req api.MultipartCompleteRequest
	if err := json.Unmarshal([]byte(f.last(t).body), &req); err != nil {
		t.Fatalf("what went out does not read back: %v", err)
	}

	for i, p := range req.Parts {
		if p.PartNumber != i+1 {
			t.Fatalf("the parts went out in the order %v", req.Parts)
		}
	}
}

// TestRenterdStorageRefusesZeroShards is the redundancy setting that would be divided by. Nothing
// is stored under it, so an answer saying zero has to be reported rather than carried further.
func TestRenterdStorageRefusesZeroShards(t *testing.T) {
	f := newFakeRenterd(t)
	f.answer("/api/bus/settings/upload", api.UploadSettings{
		Redundancy: api.RedundancySettings{MinShards: 0, TotalShards: 0},
	})

	if _, err := f.client("default").Storage(context.Background()); err == nil {
		t.Error("a redundancy of no shards at all was carried on with")
	}
}

// TestSizeFromSeeker is how an upload finds out how much it is about to send when it was not told.
// The answer becomes the content length of the request, so one that came back short would have the
// far end stop reading partway through the file and store what it had.
func TestSizeFromSeeker(t *testing.T) {
	t.Run("something that can seek", func(t *testing.T) {
		body := "the contents of the file"
		r := strings.NewReader(body)

		got, err := sizeFromSeeker(r)
		if err != nil {
			t.Fatalf("the size would not come back: %v", err)
		}
		if got != int64(len(body)) {
			t.Errorf("the size came out %d, want %d", got, len(body))
		}

		// It has to be left at the start, since what comes next is the reading of it.
		rest, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("reading what was measured: %v", err)
		}
		if string(rest) != body {
			t.Errorf("after measuring, %q is left to read, want the whole of it", rest)
		}
	})

	t.Run("something that cannot seek", func(t *testing.T) {
		// A pipe or a network stream cannot be measured without reading it, and reading it is
		// what the caller is about to do. Nothing is claimed rather than a wrong number.
		got, err := sizeFromSeeker(struct{ io.Reader }{strings.NewReader("abc")})
		if err != nil {
			t.Fatalf("something that cannot seek was answered with %v", err)
		}
		if got != 0 {
			t.Errorf("something that cannot seek was measured at %d", got)
		}
	})

	t.Run("nothing to send", func(t *testing.T) {
		got, err := sizeFromSeeker(strings.NewReader(""))
		if err != nil {
			t.Fatalf("an empty body was answered with %v", err)
		}
		if got != 0 {
			t.Errorf("an empty body was measured at %d", got)
		}
	})
}

// TestRenterdParentsDescribesTheDirectoryAndItsParent is what a listing carries as its "." and
// ".." entries. A bucket holds nothing a directory can be read out of directly, so each one is
// looked for in the listing above it - and the keys of a listing carry the leading separator,
// which the names being looked for did not, so the entries were never found and every directory
// came back as the share root.
func TestRenterdParentsDescribesTheDirectoryAndItsParent(t *testing.T) {
	f := newFakeRenterd(t)

	// The listing of the root names "dir", and the listing of "dir" names "dir/sub".
	f.answerWith(func(w http.ResponseWriter, r *http.Request) {
		res := api.ObjectsResponse{}
		switch r.URL.Path {
		case "/api/bus/objects/":
			res.Objects = []api.ObjectMetadata{{Key: "/dir/", ETag: "1111111111111111"}}
		case "/api/bus/objects/dir/":
			res.Objects = []api.ObjectMetadata{{Key: "/dir/sub/", ETag: "2222222222222222"}}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	})

	c := f.client("default")

	current, parent, err := c.Parents(context.Background(), stores.Account{}, "dir/sub")
	if err != nil {
		t.Fatalf("Parents: %v", err)
	}

	// The IDs come out of the ETags of the two entries, so finding neither leaves both at zero.
	if current.ID64 == 0 || parent.ID64 == 0 {
		t.Fatalf("the directory came back as %d and its parent as %d, want both named", current.ID64, parent.ID64)
	}
	if current.ID64 == parent.ID64 {
		t.Fatalf("the directory and its parent share the ID %d", current.ID64)
	}
}

// TestRenterdParentsOfTheRoot is the share root, which no listing holds an entry for: it stands in
// for itself and for the parent it does not have.
func TestRenterdParentsOfTheRoot(t *testing.T) {
	f := newFakeRenterd(t)
	c := f.client("default")

	current, parent, err := c.Parents(context.Background(), stores.Account{}, "")
	if err != nil {
		t.Fatalf("Parents: %v", err)
	}

	if current.ID64 == 0 {
		t.Fatal("the root came back without an ID")
	}

	// The two are the same directory, and a listing says so by leaving the parent without an ID
	// of its own.
	if parent.ID64 != 0 {
		t.Fatalf("the parent of the root came back as %d, want it left unnamed", parent.ID64)
	}
}
