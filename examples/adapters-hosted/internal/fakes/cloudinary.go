package fakes

import (
	"bytes"
	"crypto/sha1" // Cloudinary's default signature algorithm
	"encoding/hex"
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
	"time"
)

// multipartMemory is how much of an upload form ParseMultipartForm keeps
// in memory; the example's bodies are far smaller.
const multipartMemory = 1 << 20

// cloudResourceJSON is a resource as both APIs describe it, in an upload,
// rename or single-resource answer and in each entry of a list.
type cloudResourceJSON struct {
	PublicID     string `json:"public_id"`
	Bytes        int    `json:"bytes"`
	Version      int64  `json:"version"`
	CreatedAt    string `json:"created_at"`
	ResourceType string `json:"resource_type"`
}

// cloudResult is the answer to destroy: "ok" or "not found".
type cloudResult struct {
	Result string `json:"result"`
}

// cloudList is the answer to an Admin API list: always one page.
type cloudList struct {
	Resources []cloudResourceJSON `json:"resources"`
}

// cloudDeleted is the answer to an Admin API delete: every public ID it
// removed, mapped to "deleted"; Partial is always false here.
type cloudDeleted struct {
	Deleted map[string]string `json:"deleted"`
	Partial bool              `json:"partial"`
}

// cloudRawType is the only resource type the driver stores.
const cloudRawType = "raw"

// cloudRes is one stored resource.
type cloudRes struct {
	data    []byte
	version int64
	created time.Time
}

// Cloudinary is an in-process Cloudinary account: the signed Upload API
// endpoints the driver calls (raw upload, destroy, rename, download) and
// the Admin API resource endpoints it calls with Basic authentication.
// Every signature is checked with the secret. Resources are kept per
// resource type and delivery type, as Cloudinary keeps them, so an image
// uploaded outside the driver is invisible to it.
type Cloudinary struct {
	*httptest.Server
	// MaxBytes, when positive, refuses larger uploads the way Cloudinary
	// refuses a body above the plan's raw-upload limit.
	MaxBytes int

	cloud, key, secret string
	mu                 sync.Mutex
	res                map[string]*cloudRes // by "<resource type>/<delivery type>/<public id>"
	seq                int64
	adminCalls         int
	next               *Failure
}

// NewCloudinary starts an account named cloud with one API key and secret.
func NewCloudinary(cloud, key, secret string) *Cloudinary {
	f := &Cloudinary{cloud: cloud, key: key, secret: secret, res: map[string]*cloudRes{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// FailNext makes the next request answer fl.
func (f *Cloudinary) FailNext(fl Failure) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next = &fl
}

// AdminCalls reports how many Admin API requests were made, the budget
// Cloudinary limits per hour.
func (f *Cloudinary) AdminCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.adminCalls
}

// Seed stores a resource directly, as another tool (the media library, an
// upload widget) would.
func (f *Cloudinary) Seed(resourceType, deliveryType, publicID string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.res[resourceType+"/"+deliveryType+"/"+publicID] = &cloudRes{data: data, version: f.seq, created: time.Now()}
}

// PublicIDs lists the stored resources as "<resource type>/<delivery
// type>/<public id>", sorted: what the Cloudinary console would show.
func (f *Cloudinary) PublicIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.res))
	for k := range f.res {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	return ids
}

// sign is Cloudinary's documented signature, written independently of the
// driver: the signed parameters sorted, joined, the secret appended, SHA-1.
func (f *Cloudinary) sign(p url.Values) string {
	var keys []string
	for k := range p {
		switch k {
		case "file", "api_key", "signature", "resource_type", "cloud_name":
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + p.Get(k)
	}
	sum := sha1.Sum([]byte(strings.Join(parts, "&") + f.secret))
	return hex.EncodeToString(sum[:])
}

func (f *Cloudinary) describe(id string, r *cloudRes) cloudResourceJSON {
	return cloudResourceJSON{PublicID: id, Bytes: len(r.data), Version: r.version, CreatedAt: r.created.UTC().Format(time.RFC3339), ResourceType: cloudRawType}
}

func (f *Cloudinary) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.next != nil {
		fl := *f.next
		f.next = nil
		writeFailure(w, fl)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1_1/"+f.cloud+"/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if admin, ok := strings.CutPrefix(rest, "resources/"); ok {
		f.admin(w, r, admin)
		return
	}
	p, file, ok := f.params(w, r)
	if !ok {
		return
	}
	if p.Get("api_key") != f.key || p.Get("signature") != f.sign(p) {
		writeFailure(w, Failure{Status: http.StatusUnauthorized, Code: "Invalid Signature"})
		return
	}
	f.upload(w, r, rest, p, file)
}

// params reads an Upload API request's parameters and file part.
func (f *Cloudinary) params(w http.ResponseWriter, r *http.Request) (url.Values, []byte, bool) {
	switch {
	case strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data"):
		if err := r.ParseMultipartForm(multipartMemory); err != nil {
			writeFailure(w, Failure{Status: http.StatusBadRequest, Code: "bad multipart"})
			return nil, nil, false
		}
		fh := r.MultipartForm.File["file"]
		if len(fh) != 1 {
			writeFailure(w, Failure{Status: http.StatusBadRequest, Code: "Missing required parameter - file"})
			return nil, nil, false
		}
		fr, err := fh[0].Open()
		if err != nil {
			writeFailure(w, Failure{Status: http.StatusBadRequest, Code: "bad file"})
			return nil, nil, false
		}
		defer func() { _ = fr.Close() }()
		file, _ := io.ReadAll(fr)
		return url.Values(r.MultipartForm.Value), file, true
	case r.Method == http.MethodGet:
		return r.URL.Query(), nil, true
	default:
		_ = r.ParseForm()
		return r.PostForm, nil, true
	}
}

func (f *Cloudinary) upload(w http.ResponseWriter, r *http.Request, action string, p url.Values, file []byte) {
	id, at := p.Get("public_id"), "raw/"+p.Get("type")+"/"
	switch action {
	case "raw/upload":
		if f.MaxBytes > 0 && len(file) > f.MaxBytes {
			writeFailure(w, Failure{Status: http.StatusBadRequest, Code: fmt.Sprintf("File size too large. Got %d. Maximum is %d.", len(file), f.MaxBytes)})
			return
		}
		f.seq++
		f.res[at+id] = &cloudRes{data: file, version: f.seq, created: time.Now()}
		_ = json.NewEncoder(w).Encode(f.describe(id, f.res[at+id]))
	case "raw/destroy":
		result := "not found"
		if f.res[at+id] != nil {
			delete(f.res, at+id)
			result = "ok"
		}
		_ = json.NewEncoder(w).Encode(cloudResult{Result: result})
	case "raw/rename":
		from, to := p.Get("from_public_id"), p.Get("to_public_id")
		res := f.res[at+from]
		if res == nil {
			writeFailure(w, Failure{Status: http.StatusNotFound, Code: "Resource not found - " + from})
			return
		}
		delete(f.res, at+from)
		f.res[at+to] = res
		_ = json.NewEncoder(w).Encode(f.describe(to, res))
	case "raw/download":
		exp, _ := strconv.ParseInt(p.Get("expires_at"), 10, 64)
		if exp < time.Now().Unix() {
			writeFailure(w, Failure{Status: http.StatusUnauthorized, Code: "expired"})
			return
		}
		res := f.res[at+id]
		if res == nil {
			writeFailure(w, Failure{Status: http.StatusNotFound, Code: "Resource not found"})
			return
		}
		http.ServeContent(w, r, "", res.created, bytes.NewReader(res.data))
	default:
		http.NotFound(w, r)
	}
}

// admin serves /resources/<resource type>/<delivery type>[/<public id>]:
// a GET of one resource, a GET listing by prefix, or a DELETE by public
// IDs or by prefix (each answered whole, never partial).
func (f *Cloudinary) admin(w http.ResponseWriter, r *http.Request, rest string) {
	if u, pw, ok := r.BasicAuth(); !ok || u != f.key || pw != f.secret {
		writeFailure(w, Failure{Status: http.StatusUnauthorized, Code: "Invalid credentials"})
		return
	}
	f.adminCalls++
	rtype, rest, _ := strings.Cut(rest, "/")
	typ, id, _ := strings.Cut(rest, "/")
	at := rtype + "/" + typ + "/"
	if id != "" {
		res := f.res[at+id]
		if res == nil {
			writeFailure(w, Failure{Status: http.StatusNotFound, Code: "Resource not found - " + id})
			return
		}
		_ = json.NewEncoder(w).Encode(f.describe(id, res))
		return
	}
	q := r.URL.Query()
	var ids []string
	for k := range f.res {
		if pid, ok := strings.CutPrefix(k, at); ok && strings.HasPrefix(pid, q.Get("prefix")) {
			ids = append(ids, pid)
		}
	}
	sort.Strings(ids)
	if r.Method == http.MethodGet {
		out := []cloudResourceJSON{}
		for _, pid := range ids {
			out = append(out, f.describe(pid, f.res[at+pid]))
		}
		_ = json.NewEncoder(w).Encode(cloudList{Resources: out})
		return
	}
	if named := q["public_ids[]"]; len(named) > 0 {
		ids = named
	}
	gone := map[string]string{}
	for _, pid := range ids {
		if f.res[at+pid] != nil {
			delete(f.res, at+pid)
			gone[pid] = "deleted"
		}
	}
	_ = json.NewEncoder(w).Encode(cloudDeleted{Deleted: gone})
}
