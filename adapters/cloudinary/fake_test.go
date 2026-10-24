package cloudinary_test

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
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

const (
	testCloud  = "demo"
	testKey    = "123456789012345"
	testSecret = "abcdEFGHsecretXYZ"
)

type fRes struct {
	data    []byte
	version int64
	created time.Time
	typ     string
	// rtype is the resource type the resource was uploaded as.
	rtype string
}

// fake is an in-process Cloudinary serving the Upload API endpoints the
// driver signs (upload, destroy, rename, download) and the Admin API
// resource endpoints it calls with Basic authentication. It verifies every
// signature with the secret, refuses an expired download, and pages
// listings and prefix deletes in twos.
type fake struct {
	*httptest.Server
	mu  sync.Mutex
	res map[string]*fRes // by public ID
	seq int64
	// adminCalls counts Admin API requests; ranges records download Range
	// headers.
	adminCalls int
	ranges     []string
	// downloads records the parameters of every download request.
	downloads []url.Values
	// intercept, when set, sees every request first and answers it instead
	// of the fake when it returns true, to inject a failure or an odd
	// answer; it runs under mu.
	intercept func(w http.ResponseWriter, r *http.Request) bool
}

// on sets the intercept.
func (f *fake) on(h func(w http.ResponseWriter, r *http.Request) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.intercept = h
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{res: map[string]*fRes{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// sign is the documented algorithm, written independently of the driver.
func sign(p url.Values) string {
	var keys []string
	for k := range p {
		if k != "file" && k != "api_key" && k != "signature" && k != "resource_type" && k != "cloud_name" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+p.Get(k))
	}
	sum := sha1.Sum([]byte(strings.Join(parts, "&") + testSecret))
	return hex.EncodeToString(sum[:])
}

func (f *fake) fail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg}})
}

func (f *fake) json(id string, r *fRes) map[string]any {
	return map[string]any{"public_id": id, "bytes": len(r.data), "version": r.version, "created_at": r.created.UTC().Format(time.RFC3339), "resource_type": r.rtype, "type": r.typ}
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.intercept != nil && f.intercept(w, r) {
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1_1/"+testCloud+"/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if admin, ok := strings.CutPrefix(rest, "resources/"); ok {
		rt, admin, _ := strings.Cut(admin, "/")
		f.admin(w, r, rt, admin)
		return
	}
	rt, action, _ := strings.Cut(rest, "/")
	var p url.Values
	var file []byte
	switch {
	case strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data"):
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			f.fail(w, http.StatusBadRequest, "bad multipart")
			return
		}
		p = url.Values(r.MultipartForm.Value)
		fh := r.MultipartForm.File["file"]
		if len(fh) != 1 {
			f.fail(w, http.StatusBadRequest, "Missing required parameter - file")
			return
		}
		fr, _ := fh[0].Open()
		file, _ = io.ReadAll(fr)
	case r.Method == http.MethodGet:
		p = r.URL.Query()
	default:
		_ = r.ParseForm()
		p = r.PostForm
	}
	if p.Get("api_key") != testKey || p.Get("signature") != sign(p) {
		f.fail(w, http.StatusUnauthorized, "Invalid Signature")
		return
	}
	id, typ := p.Get("public_id"), p.Get("type")
	switch action {
	case "upload":
		if old := f.res[id]; old != nil && p.Get("overwrite") != "true" {
			// Cloudinary keeps the existing resource and says so.
			out := f.json(id, old)
			out["existing"] = true
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		f.seq++
		f.res[id] = &fRes{data: file, version: f.seq, created: time.Now(), typ: typ, rtype: rt}
		_ = json.NewEncoder(w).Encode(f.json(id, f.res[id]))
	case "destroy":
		result := "not found"
		if res := f.res[id]; res != nil && res.typ == typ && res.rtype == rt {
			delete(f.res, id)
			result = "ok"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"result": result})
	case "rename":
		from, to := p.Get("from_public_id"), p.Get("to_public_id")
		res := f.res[from]
		if res == nil {
			f.fail(w, http.StatusNotFound, "Resource not found - "+from)
			return
		}
		if f.res[to] != nil && p.Get("overwrite") != "true" {
			f.fail(w, http.StatusBadRequest, "to_public_id already exists")
			return
		}
		delete(f.res, from)
		f.res[to] = res
		_ = json.NewEncoder(w).Encode(f.json(to, res))
	case "download":
		f.downloads = append(f.downloads, p)
		exp, _ := strconv.ParseInt(p.Get("expires_at"), 10, 64)
		if exp < time.Now().Unix() {
			f.fail(w, http.StatusUnauthorized, "expired")
			return
		}
		res := f.res[id]
		if res == nil || res.typ != typ || res.rtype != rt {
			f.fail(w, http.StatusNotFound, "Resource not found")
			return
		}
		f.ranges = append(f.ranges, r.Header.Get("Range"))
		http.ServeContent(w, r, "", res.created, bytes.NewReader(res.data))
	default:
		http.NotFound(w, r)
	}
}

// adminPage is small so the driver must follow cursors and partial
// deletes.
const adminPage = 2

func (f *fake) admin(w http.ResponseWriter, r *http.Request, rt, rest string) {
	if u, pw, ok := r.BasicAuth(); !ok || u != testKey || pw != testSecret {
		f.fail(w, http.StatusUnauthorized, "Invalid credentials")
		return
	}
	f.adminCalls++
	typ, id, _ := strings.Cut(rest, "/")
	q := r.URL.Query()
	if id != "" {
		res := f.res[id]
		if res == nil || res.typ != typ || res.rtype != rt {
			f.fail(w, http.StatusNotFound, "Resource not found - "+id)
			return
		}
		_ = json.NewEncoder(w).Encode(f.json(id, res))
		return
	}
	var ids []string
	for k, res := range f.res {
		if res.typ == typ && res.rtype == rt && strings.HasPrefix(k, q.Get("prefix")) {
			ids = append(ids, k)
		}
	}
	sort.Strings(ids)
	switch r.Method {
	case http.MethodGet:
		start, _ := strconv.Atoi(q.Get("next_cursor"))
		end := min(start+adminPage, len(ids))
		var out []map[string]any
		for _, k := range ids[start:end] {
			out = append(out, f.json(k, f.res[k]))
		}
		page := map[string]any{"resources": out}
		if end < len(ids) {
			page["next_cursor"] = strconv.Itoa(end)
		}
		_ = json.NewEncoder(w).Encode(page)
	case http.MethodDelete:
		gone := map[string]string{}
		if named := q["public_ids[]"]; len(named) > 0 {
			for _, k := range named {
				gone[k] = "not_found"
				if f.res[k] != nil {
					delete(f.res, k)
					gone[k] = "deleted"
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"deleted": gone, "partial": false})
			return
		}
		if q.Get("prefix") == "" {
			f.fail(w, http.StatusBadRequest, "prefix required")
			return
		}
		n := min(adminPage, len(ids))
		for _, k := range ids[:n] {
			delete(f.res, k)
			gone[k] = "deleted"
		}
		out := map[string]any{"deleted": gone, "partial": len(ids) > n}
		if len(ids) > n {
			out["next_cursor"] = "more"
		}
		_ = json.NewEncoder(w).Encode(out)
	}
}
