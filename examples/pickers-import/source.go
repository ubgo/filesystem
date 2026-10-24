package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/pickers"
	"github.com/ubgo/filesystem/upload"
)

// boxIDPattern is the ID alphabet of the stand-in vendor: digits only, so
// a crafted ID cannot steer a request to another path of its API.
var boxIDPattern = regexp.MustCompile(`^[0-9]{1,32}$`)

// boxSource is a pickers.Source for a vendor the package does not ship:
// a Box-like API with GET /files/{id} for metadata and
// GET /files/{id}/content for the bytes. It names the one origin its
// token may go to, never sets credentials itself (the client Import
// passes adds the token for that origin only), and checks IDs against
// the vendor's alphabet before building a URL.
// ds:def id=boxsource-v6c3k9wn
type boxSource struct{ API string }

func (b boxSource) Origins() []string {
	u, err := url.Parse(b.API)
	if err != nil {
		return nil
	}
	return []string{u.Scheme + "://" + u.Host}
}

// Stat checks the ID, asks the vendor for the file's metadata, and reads
// at most 64 KiB of its JSON answer.
// ds:def id=boxstat-q2w7n4ck
func (b boxSource) Stat(ctx context.Context, c *http.Client, p pickers.Pick) (pickers.File, error) {
	if !boxIDPattern.MatchString(p.ID) {
		return pickers.File{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "box: malformed file ID")
	}
	resp, err := get(ctx, c, b.API+"/files/"+p.ID)
	if err != nil {
		return pickers.File{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var meta boxFile
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBoxMeta)).Decode(&meta); err != nil {
		return pickers.File{}, filesystem.Errorf(filesystem.CodeUnavailable, "box: unreadable metadata")
	}
	return pickers.File{Name: meta.Name, Size: meta.Size}, nil
}

// maxBoxMeta caps how much of the vendor's metadata answer Stat reads: a
// vendor, or whatever answers in its place, cannot make it buffer more.
const maxBoxMeta = 64 << 10

// boxFile is the stand-in vendor's file metadata: what Stat reads and
// fakeBox answers.
type boxFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func (b boxSource) Open(ctx context.Context, c *http.Client, p pickers.Pick) (io.ReadCloser, error) {
	if !boxIDPattern.MatchString(p.ID) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "box: malformed file ID")
	}
	resp, err := get(ctx, c, b.API+"/files/"+p.ID+"/content")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// get sends one GET and maps the vendor's answer to the library's codes.
func get(ctx context.Context, c *http.Client, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeUnavailable, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp, nil
	case http.StatusNotFound:
		_ = resp.Body.Close()
		return nil, filesystem.Errorf(filesystem.CodeNotFound, "box: file not found")
	case http.StatusUnauthorized, http.StatusForbidden:
		_ = resp.Body.Close()
		return nil, filesystem.Errorf(filesystem.CodePermission, "box: token refused")
	default:
		_ = resp.Body.Close()
		return nil, filesystem.Errorf(filesystem.CodeUnavailable, "box: answered %d", resp.StatusCode)
	}
}

// The stand-in vendor's file and token.
const (
	boxFileID = "4242"
	boxToken  = "box-picker-token"
)

// fakeBox answers the two calls of boxSource for one PDF, and only with
// the picker's token.
func fakeBox() *httptest.Server {
	pdf := []byte("%PDF-1.7\n% a picked contract\n")
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+boxToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/files/" + boxFileID:
			_ = json.NewEncoder(w).Encode(boxFile{Name: "contract.pdf", Size: int64(len(pdf))})
		case "/files/" + boxFileID + "/content":
			_, _ = w.Write(pdf)
		default:
			http.NotFound(w, r)
		}
	}))
}

// importFromBox runs Import with the stand-in source, as importHandler
// does for Drive, and prints what was stored.
func importFromBox(ctx context.Context, out io.Writer, disk *filesystem.Disk, fetch upload.FetchOptions, token string) error {
	box := fakeBox()
	defer box.Close()
	res, err := pickers.Import(ctx, disk, boxSource{API: box.URL}, pickers.Pick{ID: boxFileID, Token: token}, pdfPolicy, fetch)
	if err != nil {
		fmt.Fprintf(out, "box import with token %q: %s\n", token[:3]+"…", filesystem.CodeOf(err))
		return nil
	}
	fmt.Fprintf(out, "box import: %s %s %d bytes\n", res.Filename, res.ContentType, res.Size)
	return nil
}
