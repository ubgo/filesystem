package fakes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// vercelBlob is one stored blob.
type vercelBlob struct {
	pathname, contentType string
	data                  []byte
	etag                  int
	at                    time.Time
}

// Error codes the Vercel fake answers with, each sent from more than one
// place; the driver maps them to filesystem codes.
const (
	vercelForbidden = "forbidden"
	vercelNotFound  = "not_found"
)

// vercelBlobJSON is a blob as the API describes it, in a head answer, a
// put answer and each entry of a list.
type vercelBlobJSON struct {
	URL         string `json:"url"`
	Pathname    string `json:"pathname"`
	Size        int    `json:"size"`
	ContentType string `json:"contentType"`
	UploadedAt  string `json:"uploadedAt"`
	ETag        string `json:"etag"`
}

// vercelList is the answer to a list: always one page.
type vercelList struct {
	Blobs   []vercelBlobJSON `json:"blobs"`
	HasMore bool             `json:"hasMore"`
}

// Vercel is an in-process Vercel Blob store: the API (PUT ?pathname, PUT
// ?fromUrl copies, GET ?url head, GET ?prefix list, POST /delete) and a
// separate blob host serving GET with Range. The API always needs the
// bearer token; the blob host needs it only for a private store, as on
// Vercel.
type Vercel struct {
	// API and Blobs are the two hosts; pass their URLs as
	// vercelblob.Config.APIURL and BlobURL.
	API, Blobs *httptest.Server
	// MaxBytes, when positive, refuses larger bodies with file_too_large,
	// standing in for the plan's upload limit.
	MaxBytes int

	token  string
	public bool
	mu     sync.Mutex
	store  map[string]*vercelBlob
	seq    int
	next   *Failure
}

// NewVercel starts a store accepting token; public says whether its blob
// host serves without the token.
func NewVercel(token string, public bool) *Vercel {
	f := &Vercel{token: token, public: public, store: map[string]*vercelBlob{}}
	f.API = httptest.NewServer(http.HandlerFunc(f.serveAPI))
	f.Blobs = httptest.NewServer(http.HandlerFunc(f.serveBlob))
	return f
}

// Close stops both hosts.
func (f *Vercel) Close() {
	f.API.Close()
	f.Blobs.Close()
}

// FailNext makes the next authorised API request answer fl.
func (f *Vercel) FailNext(fl Failure) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next = &fl
}

// pathOf maps a blob URL on the fake's blob host to its pathname.
func (f *Vercel) pathOf(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme+"://"+u.Host != f.Blobs.URL {
		return "", false
	}
	return strings.TrimPrefix(u.Path, "/"), true
}

// eTag is the blob's quoted ETag, the value x-if-match must repeat.
func (f *Vercel) eTag(b *vercelBlob) string { return fmt.Sprintf(`"e%d"`, b.etag) }

func (f *Vercel) describe(b *vercelBlob) vercelBlobJSON {
	return vercelBlobJSON{URL: f.Blobs.URL + "/" + b.pathname, Pathname: b.pathname, Size: len(b.data), ContentType: b.contentType, UploadedAt: b.at.UTC().Format(time.RFC3339Nano), ETag: f.eTag(b)}
}

func (f *Vercel) serveAPI(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != bearer(f.token) {
		writeFailure(w, Failure{Status: http.StatusForbidden, Code: vercelForbidden})
		return
	}
	if f.next != nil {
		fl := *f.next
		f.next = nil
		writeFailure(w, fl)
		return
	}
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPut:
		f.put(w, r, q)
	case r.Method == http.MethodGet && q.Has("url"):
		p, ok := f.pathOf(q.Get("url"))
		b := f.store[p]
		if !ok || b == nil {
			writeFailure(w, Failure{Status: http.StatusNotFound, Code: vercelNotFound})
			return
		}
		_ = json.NewEncoder(w).Encode(f.describe(b))
	case r.Method == http.MethodGet:
		f.list(w, q.Get("prefix"))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/delete"):
		var in struct {
			URLs []string `json:"urls"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		for _, u := range in.URLs {
			if p, ok := f.pathOf(u); ok {
				delete(f.store, p)
			}
		}
		_, _ = w.Write([]byte("{}"))
	default:
		writeFailure(w, Failure{Status: http.StatusBadRequest, Code: "bad_request"})
	}
}

func (f *Vercel) put(w http.ResponseWriter, r *http.Request, q url.Values) {
	p := q.Get("pathname")
	existing := f.store[p]
	if m := r.Header.Get("x-if-match"); m != "" && (existing == nil || m != f.eTag(existing)) {
		writeFailure(w, Failure{Status: http.StatusPreconditionFailed, Code: "precondition_failed"})
		return
	}
	ct := r.Header.Get("x-content-type")
	var data []byte
	if from := q.Get("fromUrl"); from != "" {
		sp, ok := f.pathOf(from)
		src := f.store[sp]
		if !ok || src == nil {
			writeFailure(w, Failure{Status: http.StatusNotFound, Code: vercelNotFound})
			return
		}
		data, ct = src.data, src.contentType
	} else {
		var err error
		if data, err = io.ReadAll(r.Body); err != nil {
			return // the body did not arrive whole: nothing is stored
		}
	}
	if f.MaxBytes > 0 && len(data) > f.MaxBytes {
		writeFailure(w, Failure{Status: http.StatusRequestEntityTooLarge, Code: "file_too_large"})
		return
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	f.seq++
	b := &vercelBlob{pathname: p, contentType: ct, data: data, etag: f.seq, at: time.Now()}
	f.store[p] = b
	_ = json.NewEncoder(w).Encode(f.describe(b))
}

// list answers one page holding every match (the drivers' tests page in
// twos to exercise the cursor; the example does not need that).
func (f *Vercel) list(w http.ResponseWriter, prefix string) {
	var names []string
	for p := range f.store {
		if strings.HasPrefix(p, prefix) {
			names = append(names, p)
		}
	}
	sort.Strings(names)
	blobs := []vercelBlobJSON{}
	for _, p := range names {
		blobs = append(blobs, f.describe(f.store[p]))
	}
	_ = json.NewEncoder(w).Encode(vercelList{Blobs: blobs})
}

func (f *Vercel) serveBlob(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.public && r.Header.Get("Authorization") != bearer(f.token) {
		writeFailure(w, Failure{Status: http.StatusForbidden, Code: vercelForbidden})
		return
	}
	b := f.store[strings.TrimPrefix(r.URL.Path, "/")]
	if b == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", b.contentType)
	http.ServeContent(w, r, "", b.at, bytes.NewReader(b.data))
}
