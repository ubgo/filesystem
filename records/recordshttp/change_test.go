package recordshttp_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordshttp"
)

// onlyAdminsPublish is the rule R-I4 asks for, written as any Authorize
// would: making a file public is refused, everything else allowed.
func onlyAdminsPublish(c *recordshttp.Config, _ *[]filesystem.Option) {
	c.Authorize = func(_ *http.Request, a recordshttp.Action, t recordshttp.Target) error {
		if a == recordshttp.ActionSetVisibility && t.Change != nil && t.Change.Visibility == filesystem.VisibilityPublic {
			return errors.New("only admins publish")
		}
		return nil
	}
}

// TestChangeBeforeAuthorize pins R-I4: Authorize sees the change the
// visibility, text and set-attachment routes ask for, read before it is
// asked; a rule on it refuses publishing and allows unpublishing; every
// other route leaves Change nil.
func TestChangeBeforeAuthorize(t *testing.T) {
	e := newEnv(t)
	id := decode[recordshttp.ListResponse](t, e.upload(t, "p1", part{recordshttp.FormField, "a.txt", "a"})).Files[0].FileID
	last := func(a recordshttp.Action) *records.Change {
		t.Helper()
		for i := len(e.calls) - 1; i >= 0; i-- {
			if e.calls[i].action == a {
				return e.calls[i].target.Change
			}
		}
		t.Fatalf("Authorize was not asked for %s", a)
		return nil
	}
	wantStatus(t, e.do(http.MethodPut, "/files/"+id+"/visibility", strings.NewReader(`{"visibility":"public"}`), "application/json"), http.StatusNoContent)
	if c := last(recordshttp.ActionSetVisibility); c == nil || c.Visibility != filesystem.VisibilityPublic || c.Text != nil {
		t.Fatalf("visibility change = %+v", c)
	}
	wantStatus(t, e.do(http.MethodPut, "/files/"+id+"/text", strings.NewReader(`{"alt":"a cat","caption":"c"}`), "application/json"), http.StatusNoContent)
	if c := last(recordshttp.ActionSetText); c == nil || c.Text == nil || c.Text.Alt != "a cat" || c.Text.Caption != "c" || c.Visibility != "" {
		t.Fatalf("text change = %+v", c)
	}
	wantStatus(t, e.do(http.MethodPut, "/owners/post/p2/gallery", strings.NewReader(fmt.Sprintf(`{"file_id":%q,"previous":"trash"}`, id)), "application/json"), http.StatusOK)
	if c := last(recordshttp.ActionSetAttachment); c == nil || c.Previous != records.PreviousTrash {
		t.Fatalf("set-attachment change = %+v", c)
	}
	wantStatus(t, e.do(http.MethodPut, "/owners/post/p3/gallery", strings.NewReader(fmt.Sprintf(`{"file_id":%q}`, id)), "application/json"), http.StatusOK)
	if c := last(recordshttp.ActionSetAttachment); c == nil || c.Previous != records.PreviousKeep {
		t.Fatalf("set-attachment default change = %+v", c)
	}
	for _, c := range e.calls {
		switch c.action {
		case recordshttp.ActionSetVisibility, recordshttp.ActionSetText, recordshttp.ActionSetAttachment:
		default:
			if c.target.Change != nil {
				t.Fatalf("%s got a change: %+v", c.action, c.target.Change)
			}
		}
	}

	p := newEnv(t, onlyAdminsPublish)
	pid := decode[recordshttp.ListResponse](t, p.upload(t, "p1", part{recordshttp.FormField, "a.txt", "a"})).Files[0].FileID
	wantError(t, p.do(http.MethodPut, "/files/"+pid+"/visibility", strings.NewReader(`{"visibility":"public"}`), "application/json"), http.StatusForbidden, filesystem.CodePermission, "forbidden")
	if f, _ := p.store.File(t.Context(), tenant, pid); f.Visibility == filesystem.VisibilityPublic {
		t.Fatal("a refused publish published the file")
	}
	wantStatus(t, p.do(http.MethodPut, "/files/"+pid+"/visibility", strings.NewReader(`{"visibility":"private"}`), "application/json"), http.StatusNoContent)
}

// TestUnreadableChangeAnswersAfterAuthorize: a body that cannot be read
// reaches Authorize with Change nil; a refusing rule still answers 403,
// as it always did, and an allowing one gets the 400.
func TestUnreadableChangeAnswersAfterAuthorize(t *testing.T) {
	e := newEnv(t)
	id := decode[recordshttp.ListResponse](t, e.upload(t, "p1", part{recordshttp.FormField, "a.txt", "a"})).Files[0].FileID
	for _, route := range []struct {
		path   string
		action recordshttp.Action
	}{{"/visibility", recordshttp.ActionSetVisibility}, {"/text", recordshttp.ActionSetText}} {
		e.deny = route.action
		wantError(t, e.do(http.MethodPut, "/files/"+id+route.path, strings.NewReader(`{"visibility":`), "application/json"), http.StatusForbidden, filesystem.CodePermission, "forbidden")
		if c := e.calls[len(e.calls)-1]; c.action != route.action || c.target.Change != nil {
			t.Fatalf("%s: Authorize saw %+v", route.path, c)
		}
		e.deny = ""
		wantError(t, e.do(http.MethodPut, "/files/"+id+route.path, strings.NewReader(`{"visibility":`), "application/json"), http.StatusBadRequest, filesystem.CodeInvalidArgument, "invalid JSON body")
	}
}

// TestRespondCarriesFile pins R-I9: every success body hands Respond the
// records.File behind it (upload, list, set-attachment, replace, status),
// so a gallery can add metadata without a lookup; the default bytes hold
// no trace of it.
func TestRespondCarriesFile(t *testing.T) {
	seen := map[string]*records.File{}
	respond := func(w http.ResponseWriter, r *http.Request, status int, body recordshttp.SuccessBody) {
		switch b := body.(type) {
		case recordshttp.ListResponse:
			for _, f := range b.Files {
				seen["list:"+f.FileID] = f.File
			}
		case recordshttp.AttachedFile:
			seen["attachment"] = b.File
		case recordshttp.FileResponse:
			seen["file"] = b.File
		case recordshttp.StatusResponse:
			seen["status"] = b.File
		}
		recordshttp.JSONSuccess(w, r, status, body)
	}
	e := newEnv(t, func(c *recordshttp.Config, _ *[]filesystem.Option) { c.Respond = respond })
	rec := e.upload(t, "p1", part{recordshttp.FormField, "a.txt", "hello"})
	id := decode[recordshttp.ListResponse](t, rec).Files[0].FileID
	if f := seen["list:"+id]; f == nil || f.ID != id || f.Size != 5 || f.Scope != tenant {
		t.Fatalf("upload File = %+v", f)
	}
	delete(seen, "list:"+id)
	wantStatus(t, e.do(http.MethodGet, "/owners/post/p1/gallery", nil, ""), http.StatusOK)
	if f := seen["list:"+id]; f == nil || f.ID != id || f.Key == "" {
		t.Fatalf("list File = %+v", f)
	}
	wantStatus(t, e.do(http.MethodPut, "/owners/post/p2/gallery", strings.NewReader(fmt.Sprintf(`{"file_id":%q}`, id)), "application/json"), http.StatusOK)
	wantStatus(t, e.do(http.MethodGet, "/files/"+id+"/status", nil, ""), http.StatusOK)
	body, ct := form(t, part{recordshttp.FormField, "b.txt", "new content"})
	wantStatus(t, e.do(http.MethodPut, "/files/"+id, body, ct), http.StatusOK)
	for _, k := range []string{"attachment", "status", "file"} {
		if f := seen[k]; f == nil || f.ID != id {
			t.Fatalf("%s File = %+v", k, f)
		}
	}
	if seen["file"].Size != int64(len("new content")) {
		t.Fatalf("replace File is not the new content: %+v", seen["file"])
	}

	d := newEnv(t)
	up := d.upload(t, "p1", part{recordshttp.FormField, "a.txt", "hello"})
	for _, rec := range []string{up.Body.String(), d.do(http.MethodGet, "/owners/post/p1/gallery", nil, "").Body.String()} {
		if strings.Contains(strings.ToLower(rec), `"file"`) || strings.Contains(rec, "storage") || strings.Contains(rec, `"Key"`) {
			t.Fatalf("the default body leaks the File: %s", rec)
		}
	}
}
