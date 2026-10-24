package pickers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/pickers"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

const userToken = "user-access-token-123"

// pdf is a body whose type upload.Receive detects as application/pdf.
var pdf = []byte("%PDF-1.4\n% picked document\n")

// vendor is an in-process stand-in for the three vendors' APIs, plus a
// separate download host for Graph's redirect. Every request is recorded
// with the Authorization it carried.
type vendor struct {
	api, files *httptest.Server
	mu         sync.Mutex
	seen       []string // "host path auth"
}

func newVendor(t *testing.T) *vendor {
	t.Helper()
	v := &vendor{}
	v.files = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.record(r)
		_, _ = w.Write(pdf)
	}))
	v.api = httptest.NewServer(http.HandlerFunc(v.serve))
	t.Cleanup(v.api.Close)
	t.Cleanup(v.files.Close)
	return v
}

func (v *vendor) record(r *http.Request) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seen = append(v.seen, r.Host+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
}

// leaks returns requests to the download host that carried a token.
func (v *vendor) leaks() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []string
	for _, s := range v.seen {
		if strings.HasPrefix(s, strings.TrimPrefix(v.files.URL, "http://")) && !strings.HasSuffix(s, " ") {
			out = append(out, s)
		}
	}
	return out
}

func (v *vendor) serve(w http.ResponseWriter, r *http.Request) {
	v.record(r)
	if r.Header.Get("Authorization") != "Bearer "+userToken {
		http.Error(w, "no token", http.StatusUnauthorized)
		return
	}
	p := r.URL.Path
	switch {
	case p == "/drive/v3/files/doc1" && r.URL.Query().Get("alt") == "media":
		_, _ = w.Write(pdf)
	case p == "/drive/v3/files/doc1":
		_ = json.NewEncoder(w).Encode(map[string]string{"name": "report.pdf", "size": fmt.Sprint(len(pdf)), "mimeType": "application/pdf"})
	case p == "/drive/v3/files/gdoc":
		_ = json.NewEncoder(w).Encode(map[string]string{"name": "Notes", "mimeType": "application/vnd.google-apps.document"})
	case p == "/drive/v3/files/huge":
		_ = json.NewEncoder(w).Encode(map[string]string{"name": "huge.pdf", "size": "999999999", "mimeType": "application/pdf"})
	case p == "/2/files/get_metadata":
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["path"] != "id:abc" {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error_summary":"path/not_found/"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{".tag": "file", "name": "dropbox.pdf", "size": len(pdf)})
	case p == "/2/files/download":
		_, _ = w.Write(pdf)
	case p == "/v1.0/drives/b!drive/items/ITEM!1":
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "onedrive.pdf", "size": len(pdf), "file": map[string]string{}})
	case p == "/v1.0/drives/b!drive/items/ITEM!1/content":
		http.Redirect(w, r, v.files.URL+"/download/ITEM", http.StatusFound)
	default:
		http.NotFound(w, r)
	}
}

func (v *vendor) sources() map[string]pickers.Source {
	return map[string]pickers.Source{
		"drive":    pickers.GoogleDrive{APIURL: v.api.URL + "/drive/v3"},
		"dropbox":  pickers.Dropbox{APIURL: v.api.URL + "/2", ContentURL: v.api.URL + "/2"},
		"onedrive": pickers.OneDrive{BaseURL: v.api.URL + "/v1.0"},
	}
}

// loopback lets the guarded client reach the in-process servers; in
// production the default refuses every non-public address.
var loopback = upload.FetchOptions{AllowAddr: func(a netip.Addr) bool { return a.IsLoopback() }}

func policy() upload.Policy {
	return upload.Policy{MaxSize: 1 << 20, Allowed: []string{"application/pdf"}}
}

func newDisk(t *testing.T) *filesystem.Disk {
	t.Helper()
	d, err := filesystem.NewDisk("uploads", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestImportEachSource imports one file from each vendor through
// upload.Receive and checks the token never reached the download host.
func TestImportEachSource(t *testing.T) {
	ctx := context.Background()
	v := newVendor(t)
	picks := map[string]pickers.Pick{
		"drive":    {ID: "doc1", Token: userToken},
		"dropbox":  {ID: "id:abc", Token: userToken},
		"onedrive": {ID: "ITEM!1", DriveID: "b!drive", Token: userToken},
	}
	for name, src := range v.sources() {
		disk := newDisk(t)
		res, err := pickers.Import(ctx, disk, src, picks[name], policy(), loopback)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := disk.ReadAll(ctx, res.Key)
		if err != nil || !bytes.Equal(got, pdf) || res.ContentType != "application/pdf" || !strings.HasSuffix(res.Filename, ".pdf") {
			t.Fatalf("%s: stored %+v, %q, %v", name, res, got, err)
		}
	}
	if leaks := v.leaks(); len(leaks) != 0 {
		t.Fatalf("token sent to the download host: %q", leaks)
	}
}

// TestPolicyApplies: the upload policy refuses what it would refuse from a
// direct upload, detected from the bytes, and a reported size over the
// limit is refused before any download.
func TestPolicyApplies(t *testing.T) {
	ctx := context.Background()
	v := newVendor(t)
	drive := v.sources()["drive"]
	images := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}}
	if _, err := pickers.Import(ctx, newDisk(t), drive, pickers.Pick{ID: "doc1", Token: userToken}, images, loopback); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("a PDF under an image policy: %v, want CodeRejected", err)
	}
	before := len(v.seen)
	if _, err := pickers.Import(ctx, newDisk(t), drive, pickers.Pick{ID: "huge", Token: userToken}, policy(), loopback); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Fatalf("an oversized pick: %v, want CodeTooLarge", err)
	}
	if len(v.seen) != before+1 {
		t.Fatalf("an oversized pick was downloaded: %v", v.seen[before:])
	}
}

// TestRefusals: native documents, folders, missing files, malformed IDs
// and empty picks are refused with the right codes.
func TestRefusals(t *testing.T) {
	ctx := context.Background()
	v := newVendor(t)
	s := v.sources()
	cases := []struct {
		src  pickers.Source
		pick pickers.Pick
		want filesystem.Code
	}{
		{s["drive"], pickers.Pick{ID: "gdoc", Token: userToken}, filesystem.CodeUnsupported},
		{s["drive"], pickers.Pick{ID: "../about", Token: userToken}, filesystem.CodeInvalidArgument},
		{s["drive"], pickers.Pick{ID: "doc1"}, filesystem.CodeInvalidArgument},
		{s["dropbox"], pickers.Pick{ID: "id:missing", Token: userToken}, filesystem.CodeNotFound},
		{s["dropbox"], pickers.Pick{ID: "/etc/passwd", Token: userToken}, filesystem.CodeInvalidArgument},
		{s["onedrive"], pickers.Pick{ID: "x/../../me", Token: userToken}, filesystem.CodeInvalidArgument},
		{s["drive"], pickers.Pick{ID: "doc1", Token: "wrong"}, filesystem.CodePermission},
	}
	for _, c := range cases {
		if _, err := pickers.Import(ctx, newDisk(t), c.src, c.pick, policy(), loopback); filesystem.CodeOf(err) != c.want {
			t.Errorf("%v: %v, want %s", c.pick, err, c.want)
		}
	}
}

// TestGuardedClient: without AllowAddr the guarded client refuses the
// loopback API, so a configured or redirected endpoint cannot reach an
// internal address.
func TestGuardedClient(t *testing.T) {
	v := newVendor(t)
	_, err := pickers.Import(context.Background(), newDisk(t), v.sources()["drive"], pickers.Pick{ID: "doc1", Token: userToken}, policy(), upload.FetchOptions{})
	if err == nil {
		t.Fatal("the import reached a loopback address without AllowAddr")
	}
}

// customSource is a second Source implementation, outside the package,
// whose API redirects the download to another host: the token must stay
// on the API origin.
type customSource struct{ api string }

func (c customSource) Origins() []string { return []string{c.api} }

func (c customSource) Stat(context.Context, *http.Client, pickers.Pick) (pickers.File, error) {
	return pickers.File{Name: "custom.pdf", Size: -1}, nil
}

func (c customSource) Open(ctx context.Context, cl *http.Client, p pickers.Pick) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.api+"/redirect", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token) // a careless source: the transport must still confine it
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func TestCustomSourceTokenConfined(t *testing.T) {
	v := newVendor(t)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, v.files.URL+"/elsewhere", http.StatusFound)
	}))
	defer api.Close()
	res, err := pickers.Import(context.Background(), newDisk(t), customSource{api: api.URL}, pickers.Pick{ID: "x", Token: userToken}, policy(), loopback)
	if err != nil || res.Filename != "custom.pdf" {
		t.Fatalf("import = %+v, %v", res, err)
	}
	if leaks := v.leaks(); len(leaks) != 0 {
		t.Fatalf("token followed the redirect: %q", leaks)
	}
}

func TestPickNeverPrintsToken(t *testing.T) {
	p := pickers.Pick{ID: "doc1", Token: userToken}
	var buf bytes.Buffer
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		fmt.Fprintf(&buf, verb+"\n", p)
	}
	// Format answers every verb, so fmt never reaches GoString; call it
	// directly so a direct caller cannot see the secret either.
	buf.WriteString(p.GoString())
	buf.WriteString("\n")
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("pick", "pick", p)
	if strings.Contains(buf.String(), userToken) || !strings.Contains(buf.String(), "[redacted]") {
		t.Fatalf("token printed:\n%s", buf.String())
	}
}

// hangUp drops the connection without answering, so the client's error is
// a transport *url.Error naming the request URL.
func hangUp(w http.ResponseWriter, _ *http.Request) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// TestTransportErrorsHideDownloadURLs: when OneDrive's pre-authenticated
// download host drops the connection, the import error holds neither its
// path nor its query.
func TestTransportErrorsHideDownloadURLs(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(hangUp))
	defer dead.Close()
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/content") {
			http.Redirect(w, r, dead.URL+"/dl/SECRETPATH?tempauth=TEMPAUTH", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, `{"name":"a.pdf","size":10,"file":{}}`)
	}))
	defer graph.Close()
	_, err := pickers.Import(context.Background(), newDisk(t), pickers.OneDrive{BaseURL: graph.URL}, pickers.Pick{ID: "ITEM1", Token: userToken}, policy(), loopback)
	if err == nil || strings.Contains(err.Error(), "SECRETPATH") || strings.Contains(err.Error(), "TEMPAUTH") {
		t.Fatalf("import = %v; want an error without the download URL", err)
	}
}

// TestIDsCannotLeaveTheirPath: IDs holding "/" or "." are refused for
// every source and for a OneDrive drive ID, so "items/.." cannot reach
// another Graph path.
func TestIDsCannotLeaveTheirPath(t *testing.T) {
	v := newVendor(t)
	s := v.sources()
	cases := []struct {
		src  pickers.Source
		pick pickers.Pick
	}{
		{s["onedrive"], pickers.Pick{ID: "..", Token: userToken}},
		{s["onedrive"], pickers.Pick{ID: "a.b", Token: userToken}},
		{s["onedrive"], pickers.Pick{ID: "ITEM!1", DriveID: "..", Token: userToken}},
		{s["drive"], pickers.Pick{ID: "a.b", Token: userToken}},
		{s["dropbox"], pickers.Pick{ID: "id:..", Token: userToken}},
	}
	for _, c := range cases {
		before := len(v.seen)
		if _, err := pickers.Import(context.Background(), newDisk(t), c.src, c.pick, policy(), loopback); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%v: %v, want CodeInvalidArgument", c.pick, err)
		}
		if len(v.seen) != before {
			t.Errorf("%v reached the vendor", c.pick)
		}
	}
}
