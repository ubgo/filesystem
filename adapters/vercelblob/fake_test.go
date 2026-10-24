package vercelblob_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "vercel_blob_rw_StoreAbc123_secretsecret"

type fBlob struct {
	pathname, contentType string
	data                  []byte
	etag                  int
	at                    time.Time
}

// fake is an in-process Vercel Blob: the API (PUT ?pathname with the
// x-* headers, PUT ?fromUrl copies, GET ?url head, GET ?prefix list with a
// cursor, POST /delete) and a separate blob host serving GET with Range.
// Both require the bearer token, as a private store does.
type fake struct {
	api, blobs *httptest.Server
	mu         sync.Mutex
	store      map[string]*fBlob
	seq        int
	// deletes records the URL count of each delete request.
	deletes []int
	// unauth records requests without the token; leaked records reads of
	// a public store that carried it.
	unauth, leaked []string
	// public makes the blob host serve without the token, as a public
	// store's CDN does.
	public bool
	// intercept, when set, sees every authorised request to either host
	// first and answers it instead of the fake when it returns true, to
	// inject a failure or an odd answer; it runs under mu.
	intercept func(w http.ResponseWriter, r *http.Request) bool
}

// on sets the intercept.
func (f *fake) on(h func(w http.ResponseWriter, r *http.Request) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.intercept = h
}

func newFake(t *testing.T, public bool) *fake {
	t.Helper()
	f := &fake{store: map[string]*fBlob{}, public: public}
	f.api = httptest.NewServer(http.HandlerFunc(f.serveAPI))
	f.blobs = httptest.NewServer(http.HandlerFunc(f.serveBlob))
	t.Cleanup(f.api.Close)
	t.Cleanup(f.blobs.Close)
	return f
}

func (f *fake) fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": code}})
}

// pathOf maps a blob URL on the fake's blob host to its pathname.
func (f *fake) pathOf(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme+"://"+u.Host != f.blobs.URL {
		return "", false
	}
	return strings.TrimPrefix(u.Path, "/"), true
}

func (f *fake) json(b *fBlob) map[string]any {
	return map[string]any{"url": f.blobs.URL + "/" + b.pathname, "pathname": b.pathname, "size": len(b.data), "contentType": b.contentType, "uploadedAt": b.at.UTC().Format(time.RFC3339Nano), "etag": fmt.Sprintf(`"e%d"`, b.etag)}
}

func (f *fake) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		f.unauth = append(f.unauth, r.Method+" "+r.URL.String())
		f.fail(w, http.StatusForbidden, "forbidden")
		return false
	}
	return true
}

func (f *fake) serveAPI(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.authorized(w, r) {
		return
	}
	if r.Header.Get("x-api-version") == "" {
		f.fail(w, http.StatusBadRequest, "bad_request")
		return
	}
	if f.intercept != nil && f.intercept(w, r) {
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
			f.fail(w, http.StatusNotFound, "not_found")
			return
		}
		_ = json.NewEncoder(w).Encode(f.json(b))
	case r.Method == http.MethodGet:
		f.list(w, q)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/delete"):
		var in struct {
			URLs []string `json:"urls"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.deletes = append(f.deletes, len(in.URLs))
		for _, u := range in.URLs {
			if p, ok := f.pathOf(u); ok {
				delete(f.store, p)
			}
		}
		_, _ = w.Write([]byte("{}"))
	default:
		f.fail(w, http.StatusBadRequest, "bad_request")
	}
}

func (f *fake) put(w http.ResponseWriter, r *http.Request, q url.Values) {
	p := q.Get("pathname")
	if r.Header.Get("x-add-random-suffix") != "0" || r.Header.Get("x-allow-overwrite") != "1" || r.Header.Get("x-vercel-blob-access") == "" {
		f.fail(w, http.StatusBadRequest, "bad_request")
		return
	}
	existing := f.store[p]
	if m := r.Header.Get("x-if-match"); m != "" && (existing == nil || m != f.json(existing)["etag"]) {
		f.fail(w, http.StatusPreconditionFailed, "precondition_failed")
		return
	}
	var data []byte
	ct := r.Header.Get("x-content-type")
	if from := q.Get("fromUrl"); from != "" {
		sp, ok := f.pathOf(from)
		src := f.store[sp]
		if !ok || src == nil {
			f.fail(w, http.StatusNotFound, "not_found")
			return
		}
		data, ct = src.data, src.contentType
	} else {
		var err error
		if data, err = io.ReadAll(r.Body); err != nil {
			return // the body did not arrive whole: nothing is stored
		}
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	f.seq++
	b := &fBlob{pathname: p, contentType: ct, data: data, etag: f.seq, at: time.Now()}
	f.store[p] = b
	_ = json.NewEncoder(w).Encode(f.json(b))
}

// listPage is small so the driver must follow the cursor.
const listPage = 2

func (f *fake) list(w http.ResponseWriter, q url.Values) {
	var names []string
	for p := range f.store {
		if strings.HasPrefix(p, q.Get("prefix")) {
			names = append(names, p)
		}
	}
	sort.Strings(names)
	start, _ := strconv.Atoi(q.Get("cursor"))
	end := min(start+listPage, len(names))
	blobs := []map[string]any{{"pathname": q.Get("prefix") + "placeholder-folder/", "size": 0}}
	for _, p := range names[start:end] {
		blobs = append(blobs, f.json(f.store[p]))
	}
	out := map[string]any{"blobs": blobs, "hasMore": end < len(names)}
	if end < len(names) {
		out["cursor"] = strconv.Itoa(end)
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fake) serveBlob(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.public && r.Header.Get("Authorization") != "" {
		f.leaked = append(f.leaked, r.URL.Path)
	}
	if !f.public && !f.authorized(w, r) {
		return
	}
	if f.intercept != nil && f.intercept(w, r) {
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
