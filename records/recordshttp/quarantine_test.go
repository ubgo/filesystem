package recordshttp_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordshttp"
	"github.com/ubgo/filesystem/scan"
)

func quarantine(cfg *recordshttp.Config, _ *[]filesystem.Option) { cfg.Quarantine = true }

// TestQuarantineRoutes: with Config.Quarantine an uploaded file is
// listed without a link, refused by the file route with 409, reported
// "quarantined" by the status route and cannot be made public; a clean
// scan opens all of it, and a replacement quarantines it again.
func TestQuarantineRoutes(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, quarantine)
	wantStatus(t, e.upload(t, "p1", part{"file", "a.txt", "hello"}), http.StatusCreated)
	files := e.list(t, "p1")
	if len(files) != 1 || files[0].URL != "" || files[0].Status != records.StatusQuarantined {
		t.Fatalf("listing = %+v", files)
	}
	id := files[0].FileID
	wantError(t, e.do(http.MethodGet, "/files/"+id, nil, ""), http.StatusConflict, filesystem.CodeConflict, "")
	rec := e.do(http.MethodGet, "/files/"+id+"/status", nil, "")
	wantStatus(t, rec, http.StatusOK)
	if st := decode[recordshttp.StatusResponse](t, rec); st.Status != records.StatusQuarantined {
		t.Fatalf("status = %+v", st)
	}
	body := `{"visibility":"public"}`
	wantStatus(t, e.do(http.MethodPut, "/files/"+id+"/visibility", strings.NewReader(body), "application/json"), http.StatusConflict)

	clean := scan.ScannerFunc(func(context.Context, io.Reader) (scan.Verdict, error) { return scan.Verdict{Clean: true}, nil })
	if _, err := e.store.Scan(ctx, tenant, id, clean); err != nil {
		t.Fatal(err)
	}
	if files := e.list(t, "p1"); files[0].URL == "" {
		t.Fatal("a released file has no link")
	}
	wantStatus(t, e.do(http.MethodGet, "/files/"+id, nil, ""), http.StatusFound)

	form, ct := form(t, part{"file", "b.txt", "replaced"})
	wantStatus(t, e.do(http.MethodPut, "/files/"+id, form, ct), http.StatusOK)
	if f, _ := e.store.File(ctx, tenant, id); !f.Quarantined() {
		t.Fatalf("the replaced content is %q", f.Status)
	}
	wantStatus(t, e.do(http.MethodGet, "/files/"+id, nil, ""), http.StatusConflict)
}

// TestNoQuarantineByDefault: without Config.Quarantine uploads are ready.
func TestNoQuarantineByDefault(t *testing.T) {
	e := newEnv(t)
	wantStatus(t, e.upload(t, "p1", part{"file", "a.txt", "hello"}), http.StatusCreated)
	if files := e.list(t, "p1"); files[0].Status != records.StatusReady || files[0].URL == "" {
		t.Fatalf("listing = %+v", files)
	}
}
