package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/ubgo/filesystem"
	fsgql "github.com/ubgo/filesystem/contrib/gqlgen"
	"github.com/ubgo/filesystem/examples/gqlgen/graph"
	"github.com/ubgo/filesystem/records"
)

const (
	textBody = "quarterly numbers\n"
	htmlBody = "<!doctype html><script>alert(1)</script>"
)

// part is one file of a multipart GraphQL request.
type part struct{ name, body string }

// response is a GraphQL response with the library's error code.
type response struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []struct {
		Message    string `json:"message"`
		Extensions struct {
			Code filesystem.Code `json:"code"`
		} `json:"extensions"`
	} `json:"errors"`
}

// send posts query as a graphql-multipart-request-spec request: the
// operations field with null where each file goes, the map field naming
// those paths, and one part per file.
func send(t *testing.T, url, user, query string, variables map[string]any, paths []string, files []part) response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	ops, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		t.Fatal(err)
	}
	fileMap := map[string][]string{}
	for i, p := range paths {
		fileMap[strconv.Itoa(i)] = []string{p}
	}
	m, err := json.Marshal(fileMap)
	if err != nil {
		t.Fatal(err)
	}
	must(t, mw.WriteField("operations", string(ops)))
	must(t, mw.WriteField("map", string(m)))
	for i, f := range files {
		fw, err := mw.CreateFormFile(strconv.Itoa(i), f.name)
		must(t, err)
		_, err = io.WriteString(fw, f.body)
		must(t, err)
	}
	must(t, mw.Close())
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, &buf)
	must(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if user != "" {
		req.Header.Set(headerUser, user)
	}
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	var out response
	must(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

const (
	uploadFile  = `mutation($file: Upload!) { uploadFile(file: $file) { key filename contentType size sha256 } }`
	uploadFiles = `mutation($files: [Upload!]!) { uploadFiles(files: $files) { key } }`
	setAvatar   = `mutation($file: Upload!) { setAvatar(file: $file) { id filename contentType uploadedBy } }`
)

func TestUploads(t *testing.T) {
	h, r, err := newServer(t.Context())
	must(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	url := srv.URL + pathGraphQL
	one := map[string]any{"file": nil}

	t.Run("one file", func(t *testing.T) {
		out := send(t, url, "", uploadFile, one, []string{"variables.file"}, []part{{"report.txt", textBody}})
		var got struct{ Key, Filename, ContentType, Sha256 string }
		if len(out.Errors) != 0 || json.Unmarshal(out.Data["uploadFile"], &got) != nil || got.ContentType != "text/plain; charset=utf-8" || got.Filename != "report.txt" || got.Sha256 == "" {
			t.Fatalf("uploadFile = %+v %s", out.Errors, out.Data["uploadFile"])
		}
		if stored, err := r.Media.ReadAll(t.Context(), got.Key); err != nil || string(stored) != textBody {
			t.Fatalf("stored = %q, %v", stored, err)
		}
	})

	t.Run("html named .txt is refused", func(t *testing.T) {
		out := send(t, url, "", uploadFile, one, []string{"variables.file"}, []part{{"page.txt", htmlBody}})
		if len(out.Errors) != 1 || out.Errors[0].Extensions.Code != filesystem.CodeRejected {
			t.Fatalf("errors = %+v", out.Errors)
		}
	})

	t.Run("a list is stored all or none", func(t *testing.T) {
		two := map[string]any{"files": []any{nil, nil}}
		paths := []string{"variables.files.0", "variables.files.1"}
		before := count(t, r)
		out := send(t, url, "", uploadFiles, two, paths, []part{{"a.txt", textBody}, {"b.html", htmlBody}})
		if len(out.Errors) != 1 || out.Errors[0].Extensions.Code != filesystem.CodeRejected || count(t, r) != before {
			t.Fatalf("errors = %+v, objects %d -> %d", out.Errors, before, count(t, r))
		}
		out = send(t, url, "", uploadFiles, two, paths, []part{{"a.txt", textBody}, {"b.txt", textBody}})
		if len(out.Errors) != 0 || count(t, r) != before+2 {
			t.Fatalf("errors = %+v, objects %d -> %d", out.Errors, before, count(t, r))
		}
	})

	t.Run("avatar as a record", func(t *testing.T) {
		if out := send(t, url, "", setAvatar, one, []string{"variables.file"}, []part{{"me.txt", textBody}}); len(out.Errors) != 1 || out.Errors[0].Extensions.Code != filesystem.CodePermission {
			t.Fatalf("signed out = %+v", out.Errors)
		}
		out := send(t, url, "alice", setAvatar, one, []string{"variables.file"}, []part{{"me.txt", textBody}})
		var got struct{ ID, UploadedBy string }
		if len(out.Errors) != 0 || json.Unmarshal(out.Data["setAvatar"], &got) != nil || got.UploadedBy != "alice" {
			t.Fatalf("setAvatar = %+v %s", out.Errors, out.Data["setAvatar"])
		}
		files, err := r.Store.Attachments(t.Context(), graph.Tenant, records.Owner{Type: graph.OwnerUser, ID: "alice"}, graph.RoleAvatar)
		if err != nil || len(files) != 1 || files[0].File.ID != got.ID {
			t.Fatalf("attachments = %+v, %v", files, err)
		}
	})
}

// count returns how many objects the media disk holds.
func count(t *testing.T, r *graph.Resolver) int {
	t.Helper()
	n := 0
	for _, err := range r.Media.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		must(t, err)
		n++
	}
	return n
}

// TestSchema_IsTheLibrarysCopy fails when graph/upload.graphqls drifts from
// the schema contrib/gqlgen ships.
func TestSchema_IsTheLibrarysCopy(t *testing.T) {
	got, err := os.ReadFile("graph/upload.graphqls")
	must(t, err)
	if string(got) != fsgql.Schema {
		t.Fatal("graph/upload.graphqls differs from fsgql.Schema; copy it again")
	}
}
