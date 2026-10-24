package recordshttp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/records/recordshttp"
)

// problem is an application's own error envelope, the shape a custom
// ErrorWriter writes in TestErrorWriter.
type problem struct {
	Status int             `json:"status"`
	Code   filesystem.Code `json:"code"`
	Detail string          `json:"detail"`
}

// problemErrors returns an ErrorWriter answering application/problem+json
// and the causes it was given, in order.
func problemErrors(causes *[]error) fshttp.ErrorWriter {
	return func(w http.ResponseWriter, _ *http.Request, status int, err error) {
		*causes = append(*causes, err)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(problem{Status: status, Code: filesystem.CodeOf(err), Detail: fshttp.ClientMessage(err)})
	}
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, code filesystem.Code) problem {
	t.Helper()
	wantStatus(t, rec, status)
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q: the default writer answered", ct)
	}
	var p problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != status || p.Code != code {
		t.Fatalf("problem = %+v, want status %d code %s", p, status, code)
	}
	return p
}

// TestErrorWriter (B5): Config.Errors answers every refusal in the
// application's envelope (a storage error, the 401 and 403 of the hooks,
// a 412, the request cap) and gets each cause for its logs, the hooks'
// own errors included.
func TestErrorWriter(t *testing.T) {
	var causes []error
	e := newEnv(t, func(c *recordshttp.Config, _ *[]filesystem.Option) { c.Errors = problemErrors(&causes) })

	decodeProblem(t, e.do(http.MethodGet, "/files/fil_missing", nil, ""), http.StatusNotFound, filesystem.CodeNotFound)

	rec := e.upload(t, "p1", part{"file", "a.txt", "hello"})
	wantStatus(t, rec, http.StatusCreated)
	id := decode[recordshttp.ListResponse](t, rec).Files[0].FileID

	e.deny = recordshttp.ActionList
	decodeProblem(t, e.do(http.MethodGet, "/owners/post/p1/gallery", nil, ""), http.StatusForbidden, filesystem.CodePermission)
	if last := causes[len(causes)-1]; !strings.Contains(last.Error(), "not your post") {
		t.Fatalf("the 403's cause lost Authorize's error: %v", last)
	}
	e.deny = ""

	req := httptest.NewRequest(http.MethodDelete, "/files/"+id, nil)
	req.Header.Set("If-Match", `"stale"`)
	stale := httptest.NewRecorder()
	e.srv.ServeHTTP(stale, req)
	decodeProblem(t, stale, http.StatusPreconditionFailed, filesystem.CodeConflict)

	big := e.upload(t, "p1", part{"file", "big.txt", strings.Repeat("x", 4<<10)})
	decodeProblem(t, big, http.StatusRequestEntityTooLarge, filesystem.CodeTooLarge)

	e.anon = true
	decodeProblem(t, e.do(http.MethodGet, "/files/"+id, nil, ""), http.StatusUnauthorized, filesystem.CodePermission)
	if last := causes[len(causes)-1]; !strings.Contains(last.Error(), "no session") {
		t.Fatalf("the 401's cause lost Scope's error: %v", last)
	}
	if len(causes) != 5 {
		t.Fatalf("the writer saw %d failures, want 5", len(causes))
	}
}

// TestDefaultErrorsUnchanged: without Config.Errors the answers keep the
// ErrorResponse JSON and messages.
func TestDefaultErrorsUnchanged(t *testing.T) {
	e := newEnv(t)
	wantError(t, e.do(http.MethodGet, "/files/fil_missing", nil, ""), http.StatusNotFound, filesystem.CodeNotFound, "Not Found")
	e.anon = true
	wantError(t, e.do(http.MethodGet, "/files/fil_missing", nil, ""), http.StatusUnauthorized, filesystem.CodePermission, "not signed in")
	e.anon, e.deny = false, recordshttp.ActionList
	wantError(t, e.do(http.MethodGet, "/owners/post/p1/gallery", nil, ""), http.StatusForbidden, filesystem.CodePermission, "forbidden")
}

// TestListPages (I20): ?limit= and ?cursor= page an owner's files with a
// next cursor; without them the route lists everything, as before; a
// limit that is not a number is 400.
func TestListPages(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		wantStatus(t, e.upload(t, "p1", part{"file", name, "hello " + name}), http.StatusCreated)
	}
	page := func(query string) recordshttp.ListResponse {
		t.Helper()
		rec := e.do(http.MethodGet, "/owners/post/p1/gallery?"+query, nil, "")
		wantStatus(t, rec, http.StatusOK)
		return decode[recordshttp.ListResponse](t, rec)
	}
	first := page("limit=2")
	if len(first.Files) != 2 || first.NextCursor == "" || first.Files[0].Filename != "a.txt" {
		t.Fatalf("first page: %+v", first)
	}
	second := page("limit=2&cursor=" + url.QueryEscape(first.NextCursor))
	if len(second.Files) != 1 || second.NextCursor != "" || second.Files[0].Filename != "c.txt" {
		t.Fatalf("second page: %+v", second)
	}
	all := page("")
	if len(all.Files) != 3 || all.NextCursor != "" {
		t.Fatalf("unpaged list: %+v", all)
	}
	if strings.Contains(e.do(http.MethodGet, "/owners/post/p1/gallery", nil, "").Body.String(), "next_cursor") {
		t.Fatal("an unpaged answer carries next_cursor")
	}
	wantError(t, e.do(http.MethodGet, "/owners/post/p1/gallery?limit=two", nil, ""), http.StatusBadRequest, filesystem.CodeInvalidArgument, "limit")
	wantError(t, e.do(http.MethodGet, "/owners/post/p1/gallery?cursor=%21", nil, ""), http.StatusBadRequest, filesystem.CodeInvalidArgument, "cursor")
}
