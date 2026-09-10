package main

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/mike76-dev/sombrero/smb2"
)

// The sharing rule as clients meet it. The tests in sharing_test.go take the rule apart one right at
// a time; the ones here put together the opens that Explorer, the Linux clients and the Office
// applications actually make of one file or folder, from clients of their own, and hold the answers
// against what a Windows server gives. A rule that passes the first set and fails these refuses a
// real client somewhere, which is how the attribute opens got through: every unit of the rule was
// tested, and none of the opens a client makes. It turned Explorer away from the share root on
// Windows and Nautilus away from the files of a folder on Ubuntu, from the same mistake.

// sharedOpen is an open as a client makes it: what it asks to do, and what it will put up with from
// everybody else.
type sharedOpen struct {
	name    string
	access  uint32
	sharing uint32
}

const attributesOnly = smb2.FILE_READ_ATTRIBUTES | smb2.SYNCHRONIZE

var (
	// What a client opens a file or folder with to show it in a listing, to learn its size, or to
	// answer a stat: the attributes and nothing else. Explorer and the Linux clients make these all
	// the time, and not always under a permissive mode, since the file system never weighs them.
	statOpen          = sharedOpen{"an attribute query", attributesOnly, everySharing}
	statOpenExclusive = sharedOpen{"an attribute query that shares nothing", attributesOnly, 0}

	// What the properties dialog and the security tab ask for: the security descriptor.
	securityQuery = sharedOpen{"a security query", smb2.READ_CONTROL | smb2.SYNCHRONIZE, 0}

	// A folder being listed, and the one Explorer keeps open on it to be told of changes.
	listFolder  = sharedOpen{"a folder listing", smb2.FILE_LIST_DIRECTORY | attributesOnly, smb2.FILE_SHARE_READ | smb2.FILE_SHARE_WRITE}
	watchFolder = sharedOpen{"a folder watch", smb2.FILE_LIST_DIRECTORY | smb2.SYNCHRONIZE, everySharing}

	// A client reading a file under the most permissive mode there is: a copy out of the share, a
	// Linux client opening it, Nautilus looking into it to tell what it is.
	reader = sharedOpen{"a reader", smb2.FILE_READ_DATA | smb2.FILE_READ_ATTRIBUTES | smb2.SYNCHRONIZE, everySharing}

	// An application holding a document it is editing: it reads and writes it, and lets everybody
	// else read it but neither write nor delete it underneath.
	editor = sharedOpen{"an editor", smb2.FILE_READ_DATA | smb2.FILE_WRITE_DATA | smb2.FILE_READ_ATTRIBUTES | smb2.SYNCHRONIZE, smb2.FILE_SHARE_READ}

	// A client writing a file it means to have to itself until it is done.
	exclusiveWriter = sharedOpen{"an exclusive writer", smb2.FILE_WRITE_DATA | smb2.FILE_APPEND_DATA | smb2.FILE_READ_ATTRIBUTES | smb2.SYNCHRONIZE, 0}

	// Explorer deleting or renaming what it was shown.
	deleter = sharedOpen{"a delete", smb2.DELETE | smb2.FILE_READ_ATTRIBUTES | smb2.SYNCHRONIZE, everySharing}
)

// openShared makes the open on the path, as a directory if dir is set, and returns the answer.
func (cl *testClient) openShared(path string, dir bool, o sharedOpen) []byte {
	cl.h.t.Helper()

	var options uint32
	if dir {
		options = smb2.FILE_DIRECTORY_FILE
	}

	cl.mid++
	resp, err := cl.send(createRequestSharing(cl.mid, cl.ss.sessionID, cl.tc.treeID, path,
		smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, o.access, options, o.sharing, nil))
	if err != nil {
		cl.h.t.Fatalf("%s of %q: %v", o.name, path, err)
	}

	return resp.Encode()
}

// sharingStep is one open in a scenario: which of its clients makes it, and what it is answered.
type sharingStep struct {
	client int
	open   sharedOpen
	want   uint32
}

// runSharingScenario makes the steps in order against one path, each from the client it names, and
// keeps every open it is granted: what a step is weighed against is everything granted before it.
func runSharingScenario(t *testing.T, path string, dir bool, steps []sharingStep) {
	t.Helper()

	h := newSMBTest(t)
	if dir {
		if path != "" {
			h.files.putDir(path)
		}
	} else {
		h.files.put(path, 1024)
	}

	// Two users on connections of their own, which is how the opens of two people meet.
	clients := []*testClient{h.dial("alice"), h.dial("bob")}

	for i, s := range steps {
		status := smb2.Header(clients[s.client].openShared(path, dir, s.open)).Status()
		if status != s.want {
			t.Fatalf("step %d, %s from client %d: answered %#x, want %#x", i+1, s.open.name, s.client, status, s.want)
		}
	}
}

func TestSharingBetweenClients(t *testing.T) {
	const (
		ok      = smb2.STATUS_OK
		refused = smb2.STATUS_SHARING_VIOLATION
	)

	files := []struct {
		name  string
		steps []sharingStep
	}{
		// What the file system lets through, and a server that refused it breaks a client.
		{"a listing of a file somebody is editing", []sharingStep{
			{0, editor, ok},
			{1, statOpen, ok},
		}},
		{"an attribute query that shares nothing, then a reader", []sharingStep{
			{0, statOpenExclusive, ok},
			{1, reader, ok},
		}},
		{"a reader, then an attribute query that shares nothing", []sharingStep{
			{0, reader, ok},
			{1, statOpenExclusive, ok},
		}},
		{"the size of a file being written exclusively", []sharingStep{
			{0, exclusiveWriter, ok},
			{1, statOpenExclusive, ok},
		}},
		{"the properties of a file being written exclusively", []sharingStep{
			{0, exclusiveWriter, ok},
			{1, securityQuery, ok},
		}},
		{"a look into a document somebody is editing", []sharingStep{
			{0, editor, ok},
			{1, reader, ok},
		}},
		{"many readers", []sharingStep{
			{0, reader, ok},
			{1, reader, ok},
			{0, reader, ok},
			{1, statOpenExclusive, ok},
		}},

		// What the file system refuses, and a server that let it through loses somebody's work.
		{"a second editor of an open document", []sharingStep{
			{0, editor, ok},
			{1, editor, refused},
		}},
		{"deleting a document somebody is editing", []sharingStep{
			{0, editor, ok},
			{1, deleter, refused},
		}},
		{"reading a file being written exclusively", []sharingStep{
			{0, exclusiveWriter, ok},
			{1, reader, refused},
		}},
		{"an exclusive writer where somebody is reading", []sharingStep{
			{0, reader, ok},
			{1, exclusiveWriter, refused},
		}},
		{"an attribute query does not let a writer through", []sharingStep{
			{0, editor, ok},
			{1, statOpenExclusive, ok},
			{1, editor, refused},
		}},
	}

	for _, tt := range files {
		t.Run("file/"+tt.name, func(t *testing.T) {
			runSharingScenario(t, "report.docx", false, tt.steps)
		})
	}

	// The same opens meet on folders, the share root among them: it is the one Explorer opens
	// first and most often, and the one a refusal makes the whole drive inaccessible over.
	folders := []struct {
		name  string
		steps []sharingStep
	}{
		{"a listing of a watched folder", []sharingStep{
			{0, watchFolder, ok},
			{1, listFolder, ok},
		}},
		{"a listing beside an attribute query that shares nothing", []sharingStep{
			{0, statOpenExclusive, ok},
			{1, listFolder, ok},
			{0, listFolder, ok},
		}},
		{"an attribute query beside a listing", []sharingStep{
			{0, listFolder, ok},
			{1, statOpenExclusive, ok},
			{1, statOpen, ok},
		}},
		{"Explorer browsing while another client does the same", []sharingStep{
			{0, watchFolder, ok},
			{0, statOpenExclusive, ok},
			{0, listFolder, ok},
			{1, watchFolder, ok},
			{1, statOpenExclusive, ok},
			{1, listFolder, ok},
			{1, securityQuery, ok},
		}},
	}

	for _, where := range []struct{ label, path string }{{"folder", "docs"}, {"root", ""}} {
		for _, tt := range folders {
			t.Run(where.label+"/"+tt.name, func(t *testing.T) {
				runSharingScenario(t, where.path, true, tt.steps)
			})
		}
	}
}

// TestLinuxClientReadsAFolderWindowsIsShowing is the Ubuntu report: Nautilus opening a folder that
// a Windows client has open in Explorer at the same time. Nautilus lists the folder and then reads
// into every file to tell what it is, and a refusal of any one of those reads is what it reports as
// "could not display all the contents: Device or resource busy". Only a file somebody is actually
// writing may be refused; one Explorer is merely showing may not.
func TestLinuxClientReadsAFolderWindowsIsShowing(t *testing.T) {
	files := []string{"docs/a.txt", "docs/b.txt", "docs/c.txt"}

	for _, tt := range []struct {
		name     string
		windows  map[string]sharedOpen
		refusals map[string]bool
	}{
		{
			"Explorer is showing the folder",
			map[string]sharedOpen{"docs/b.txt": statOpenExclusive, "docs/c.txt": securityQuery},
			nil,
		},
		{
			"somebody is editing a document in it",
			map[string]sharedOpen{"docs/a.txt": editor, "docs/b.txt": statOpenExclusive},
			nil,
		},
		{
			"a file in it is being written exclusively",
			map[string]sharedOpen{"docs/b.txt": exclusiveWriter, "docs/c.txt": statOpenExclusive},
			map[string]bool{"docs/b.txt": true},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newSMBTest(t)
			h.files.putDir("docs")
			for _, f := range files {
				h.files.put(f, 1024)
			}

			windows, linux := h.dial("alice"), h.dial("bob")

			if status := smb2.Header(windows.openShared("docs", true, watchFolder)).Status(); status != smb2.STATUS_OK {
				t.Fatalf("Explorer's watch on the folder was answered %#x", status)
			}
			for path, o := range tt.windows {
				if status := smb2.Header(windows.openShared(path, false, o)).Status(); status != smb2.STATUS_OK {
					t.Fatalf("the Windows client's %s of %s was answered %#x", o.name, path, status)
				}
			}

			if status := smb2.Header(linux.openShared("docs", true, listFolder)).Status(); status != smb2.STATUS_OK {
				t.Fatalf("Nautilus's listing of the folder was answered %#x", status)
			}
			for _, f := range files {
				want := uint32(smb2.STATUS_OK)
				if tt.refusals[f] {
					want = smb2.STATUS_SHARING_VIOLATION
				}
				if status := smb2.Header(linux.openShared(f, false, reader)).Status(); status != want {
					t.Errorf("Nautilus reading %s was answered %#x, want %#x", f, status, want)
				}
			}
		})
	}
}

// shareAccessModel is the sharing rule the way Windows keeps it: not a pair of opens at a time but
// counts over every open of the file that reads, writes or deletes it. It is written apart from the
// rule in sharing.go on purpose - the two are meant to agree, and a test that ran the server's own
// rule against itself would agree with any mistake in it.
type shareAccessModel struct {
	opens, readers, writers, deleters     int
	sharedRead, sharedWrite, sharedDelete int
}

// sharingOf takes an open apart into what the model counts.
func sharingOf(o sharedOpen) (read, write, del, shareRead, shareWrite, shareDelete bool) {
	read = o.access&(smb2.FILE_READ_DATA|smb2.FILE_EXECUTE|smb2.GENERIC_READ) != 0
	write = o.access&(smb2.FILE_WRITE_DATA|smb2.FILE_APPEND_DATA|smb2.GENERIC_WRITE) != 0
	del = o.access&smb2.DELETE != 0
	return read, write, del, o.sharing&smb2.FILE_SHARE_READ != 0, o.sharing&smb2.FILE_SHARE_WRITE != 0, o.sharing&smb2.FILE_SHARE_DELETE != 0
}

// allows reports whether the open may be made beside the ones counted.
func (m *shareAccessModel) allows(o sharedOpen) bool {
	read, write, del, shR, shW, shD := sharingOf(o)
	if !read && !write && !del {
		return true
	}

	return !(read && m.sharedRead < m.opens ||
		write && m.sharedWrite < m.opens ||
		del && m.sharedDelete < m.opens ||
		m.readers > 0 && !shR ||
		m.writers > 0 && !shW ||
		m.deleters > 0 && !shD)
}

// count adds the open to the counts, or takes it off them with n = -1. An open that neither reads,
// writes nor deletes is never counted.
func (m *shareAccessModel) count(o sharedOpen, n int) {
	read, write, del, shR, shW, shD := sharingOf(o)
	if !read && !write && !del {
		return
	}

	b := func(v bool) int {
		if v {
			return n
		}
		return 0
	}
	m.opens += n
	m.readers += b(read)
	m.writers += b(write)
	m.deleters += b(del)
	m.sharedRead += b(shR)
	m.sharedWrite += b(shW)
	m.sharedDelete += b(shD)
}

// TestSharingAgreesWithTheModel makes long runs of opens and closes of one file, from several
// clients, and holds every answer against the model. The opens are drawn from what clients make
// together with every mode an open can name, so that it covers the combinations nobody thought to
// write a scenario for. The runs are seeded, and a failure names its seed and every step before it.
func TestSharingAgreesWithTheModel(t *testing.T) {
	var pool []sharedOpen
	for _, o := range []sharedOpen{statOpen, securityQuery, reader, editor, exclusiveWriter, deleter} {
		for sharing := uint32(0); sharing <= everySharing; sharing++ {
			pool = append(pool, sharedOpen{fmt.Sprintf("%s sharing %#x", o.name, sharing), o.access, sharing})
		}
	}
	pool = append(pool,
		sharedOpen{"a generic reader", smb2.GENERIC_READ, everySharing},
		sharedOpen{"a generic writer sharing reads", smb2.GENERIC_WRITE, smb2.FILE_SHARE_READ},
	)

	type held struct {
		client int
		fid    []byte
		open   sharedOpen
	}

	const (
		runs  = 25
		steps = 40
	)

	for seed := int64(1); seed <= runs; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			h := newSMBTest(t)
			h.files.put("file", 1024)
			clients := []*testClient{h.dial("alice"), h.dial("bob"), h.dial("alice")}

			var model shareAccessModel
			var open []held
			var log []string

			for i := 0; i < steps; i++ {
				// A close now and then, so that the file frees up and the counts go both ways.
				if len(open) > 0 && rng.Intn(4) == 0 {
					k := rng.Intn(len(open))
					victim := open[k]
					if _, err := clients[victim.client].closeHandle(victim.fid); err != nil {
						t.Fatalf("close: %v", err)
					}
					model.count(victim.open, -1)
					open = append(open[:k], open[k+1:]...)
					log = append(log, fmt.Sprintf("client %d closes %s", victim.client, victim.open.name))
					continue
				}

				o := pool[rng.Intn(len(pool))]
				c := rng.Intn(len(clients))
				want := model.allows(o)

				buf := clients[c].openShared("file", false, o)
				status := smb2.Header(buf).Status()
				log = append(log, fmt.Sprintf("client %d opens %s: %#x", c, o.name, status))

				if got := status == smb2.STATUS_OK; got != want {
					t.Fatalf("seed %d, step %d: %s from client %d was answered %#x, want it granted: %v\n%s",
						seed, i+1, o.name, c, status, want, strings.Join(log, "\n"))
				}
				if status != smb2.STATUS_OK && status != smb2.STATUS_SHARING_VIOLATION {
					t.Fatalf("seed %d, step %d: %s was answered %#x, which is no sharing answer\n%s",
						seed, i+1, o.name, status, strings.Join(log, "\n"))
				}

				if status == smb2.STATUS_OK {
					model.count(o, 1)
					open = append(open, held{c, createdFileID(buf), o})
				}
			}
		})
	}
}
