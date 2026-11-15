package files

import (
	"context"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
	"golang.org/x/net/webdav"
)

// WebDAV serves the shares as a drive that an operating system mounts over
// HTTP, with the share credentials sent as HTTP Basic authentication: the
// shares are the folders under /dav, and the rights are the SMB rights.

// davPrefix is where the server mounts the WebDAV handler.
const davPrefix = "/dav"

// davSession is the context key the authenticated session travels under,
// since the file system interface knows nothing of requests.
type davSession struct{}

// DAV returns the WebDAV handler, which the server mounts at /dav. This
// version serves reading alone; the calls that write are refused.
func (h *Handler) DAV() http.Handler {
	wd := &webdav.Handler{
		Prefix:     davPrefix,
		FileSystem: davFS{h: h},
		LockSystem: webdav.NewMemLS(),
		Logger: func(req *http.Request, err error) {
			if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrPermission) {
				log.Printf("WebDAV: %s %s: %v", req.Method, req.URL.Path, err)
			}
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		s, ok := h.basicAuth(req)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="Sombrero"`)
			writeError(w, "authentication required", http.StatusUnauthorized)
			return
		}
		switch req.Method {
		case http.MethodPut, http.MethodDelete, "MKCOL", "MOVE", "COPY", "PROPPATCH":
			writeError(w, "WebDAV is read-only on this server", http.StatusForbidden)
			return
		}
		wd.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), davSession{}, s)))
	})
}

// basicAuth reads the share credentials from the request: the username with
// the workgroup as workgroup\username, workgroup/username or
// username@workgroup, and the password, empty for a guest.
func (h *Handler) basicAuth(req *http.Request) (session, bool) {
	user, password, ok := req.BasicAuth()
	if !ok {
		return session{}, false
	}
	var workgroup, username string
	switch {
	case strings.Contains(user, `\`):
		workgroup, username, _ = strings.Cut(user, `\`)
	case strings.Contains(user, "/"):
		workgroup, username, _ = strings.Cut(user, "/")
	case strings.Contains(user, "@"):
		username, workgroup, _ = strings.Cut(user, "@")
	}
	if workgroup == "" || username == "" {
		return session{}, false
	}

	wg, err := h.resolveWorkgroup(workgroup)
	if err != nil {
		log.Printf("WebDAV: failed to find workgroup: %v", err)
		return session{}, false
	}
	if wg.ID == 0 {
		return session{}, false
	}
	acc, err := h.store.FindAccount(username, wg.UUID.String())
	if err != nil {
		if !errors.Is(err, stores.ErrAccountNotFound) {
			log.Printf("WebDAV: failed to find account: %v", err)
		}
		return session{}, false
	}
	if !acc.CheckPassword(password) {
		return session{}, false
	}
	return session{account: acc, workgroup: wg, expires: h.now().Add(sessionLifetime)}, true
}

// davFS is the shares as a file system: the root holds a folder per share
// the account may read, and each share its files and folders.
type davFS struct {
	h *Handler
}

func (fs davFS) Mkdir(context.Context, string, os.FileMode) error { return os.ErrPermission }
func (fs davFS) RemoveAll(context.Context, string) error          { return os.ErrPermission }
func (fs davFS) Rename(context.Context, string, string) error     { return os.ErrPermission }

// place is where a WebDAV path points: the root, a share, or a file or
// folder on it, with the client that reaches the share.
type place struct {
	share    stores.Share
	key      string
	client   client.Client
	partSize uint64
	session  session
}

// resolve finds what the path names, as far as the account may see it. The
// root and a share need no lookup beyond the rights; anything deeper is
// looked up on the share.
func (fs davFS) resolve(ctx context.Context, name string) (place, davInfo, error) {
	s, ok := ctx.Value(davSession{}).(session)
	if !ok {
		return place{}, davInfo{}, os.ErrPermission
	}
	name = path.Clean("/" + name)
	if name == "/" {
		return place{session: s}, davInfo{name: "/", dir: true, modified: fs.h.now()}, nil
	}

	shareName, rest, _ := strings.Cut(strings.TrimPrefix(name, "/"), "/")
	share, err := fs.h.store.GetShare(shareName)
	if err != nil {
		return place{}, davInfo{}, err
	}
	if share.Name == "" {
		return place{}, davInfo{}, os.ErrNotExist
	}
	r, err := fs.h.rightsOn(s.account, share)
	if err != nil {
		return place{}, davInfo{}, err
	}
	if !r.Read {
		return place{}, davInfo{}, os.ErrNotExist
	}
	c, partSize, err := fs.h.server.ShareClient(share, s.account.Workgroup)
	if err != nil {
		return place{}, davInfo{}, err
	}
	p := place{share: share, key: rest, client: c, partSize: partSize, session: s}
	if rest == "" {
		return p, davInfo{name: shareName, dir: true, modified: fs.h.now()}, nil
	}

	oi, err := c.Object(ctx, s.account, rest)
	if errors.Is(err, stores.ErrNotFound) {
		return place{}, davInfo{}, os.ErrNotExist
	} else if err != nil {
		return place{}, davInfo{}, err
	}
	return p, infoOf(oi), nil
}

// infoOf describes a listed object the way the file system does.
func infoOf(oi client.ObjectInfo) davInfo {
	key := strings.TrimSuffix(oi.Key, "/")
	return davInfo{name: path.Base(key), dir: strings.HasSuffix(oi.Key, "/"), size: oi.Size, modified: oi.ModifiedAt}
}

func (fs davFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	_, info, err := fs.resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	return info, nil
}

// OpenFile opens a file for reading, or a folder for listing; nothing is
// opened for writing.
func (fs davFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		return nil, os.ErrPermission
	}
	p, info, err := fs.resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	f := &davFile{fs: fs, place: p, info: info}
	if !info.dir {
		f.reader = &objectReader{
			fetch: func(offset, length uint64, w io.Writer) error {
				return p.client.Read(ctx, p.session.account, p.key, offset, length, w)
			},
			path:  p.key,
			size:  info.size,
			chunk: fs.h.chunkSize(p.partSize),
		}
	}
	return f, nil
}

// davInfo is what the file system says about a file or a folder.
type davInfo struct {
	name     string
	dir      bool
	size     uint64
	modified time.Time
}

func (i davInfo) Name() string       { return i.name }
func (i davInfo) Size() int64        { return int64(i.size) }
func (i davInfo) ModTime() time.Time { return i.modified }
func (i davInfo) IsDir() bool        { return i.dir }
func (i davInfo) Sys() any           { return nil }

func (i davInfo) Mode() os.FileMode {
	if i.dir {
		return os.ModeDir | 0o555
	}
	return 0o444
}

// ContentType names the type by the extension, so that a listing does not
// read the head of every file to find out.
func (i davInfo) ContentType(context.Context) (string, error) {
	if i.dir {
		return "httpd/unix-directory", nil
	}
	if ctype := mime.TypeByExtension(path.Ext(i.name)); ctype != "" {
		return ctype, nil
	}
	return "application/octet-stream", nil
}

// davFile is an open file or folder.
type davFile struct {
	fs     davFS
	place  place
	info   davInfo
	reader *objectReader

	children []os.FileInfo
	listed   bool
}

func (f *davFile) Close() error                       { return nil }
func (f *davFile) Stat() (os.FileInfo, error)         { return f.info, nil }
func (f *davFile) Write([]byte) (int, error)          { return 0, os.ErrPermission }
func (f *davFile) Seek(o int64, w int) (int64, error) { return f.readerOf().Seek(o, w) }
func (f *davFile) Read(p []byte) (int, error)         { return f.readerOf().Read(p) }

// readerOf is the file's reader, or one that reads nothing for a folder.
func (f *davFile) readerOf() io.ReadSeeker {
	if f.reader == nil {
		return strings.NewReader("")
	}
	return f.reader
}

// Readdir lists a folder: the shares at the root, the entries of a folder
// otherwise, in the batches the caller asks for.
func (f *davFile) Readdir(count int) ([]os.FileInfo, error) {
	if !f.info.dir {
		return nil, errors.New("not a folder")
	}
	if !f.listed {
		children, err := f.list()
		if err != nil {
			return nil, err
		}
		f.children, f.listed = children, true
	}
	if count <= 0 {
		all := f.children
		f.children = nil
		return all, nil
	}
	if len(f.children) == 0 {
		return nil, io.EOF
	}
	n := min(count, len(f.children))
	batch := f.children[:n]
	f.children = f.children[n:]
	return batch, nil
}

func (f *davFile) list() ([]os.FileInfo, error) {
	ctx := context.Background()
	s := f.place.session
	if f.place.client == nil {
		shares, err := f.fs.h.store.GetShares(s.account)
		if err != nil {
			return nil, err
		}
		var infos []os.FileInfo
		for _, share := range shares {
			r, err := f.fs.h.rightsOn(s.account, share)
			if err != nil {
				return nil, err
			}
			if r.Read {
				infos = append(infos, davInfo{name: share.Name, dir: true, modified: f.fs.h.now()})
			}
		}
		return infos, nil
	}

	prefix := ""
	if f.place.key != "" {
		prefix = f.place.key + "/"
	}
	ois, err := f.place.client.List(ctx, s.account, prefix)
	if err != nil {
		return nil, err
	}
	infos := make([]os.FileInfo, 0, len(ois))
	for _, oi := range ois {
		if info := infoOf(oi); info.name != "" && info.name != "/" && info.name != "." {
			infos = append(infos, info)
		}
	}
	return infos, nil
}
