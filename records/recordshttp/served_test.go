package recordshttp_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordshttp"
)

// meter is a records.ServeHooks plugin standing in for egress metering or
// an access log; err makes it refuse.
type meter struct {
	served []records.Served
	err    error
}

func (m *meter) Name() string { return "meter" }

func (m *meter) OnServed(_ context.Context, _ *records.Store, sv records.Served) error {
	m.served = append(m.served, sv)
	return m.err
}

// The file route reports every link it hands out, and hands out none it
// could not report.
func TestReadReportsServed(t *testing.T) {
	m := &meter{}
	e := newEnvWith(t, []records.Plugin{m})
	af := decode[recordshttp.ListResponse](t, e.upload(t, "p1", part{field: recordshttp.FormField, filename: "a.txt", body: "abc"})).Files[0]

	wantStatus(t, e.do(http.MethodGet, "/files/"+af.FileID, nil, ""), http.StatusFound)
	if len(m.served) != 1 || m.served[0].Kind != records.ServeLink || m.served[0].FileID != af.FileID || m.served[0].Scope != tenant || m.served[0].Bytes != 3 {
		t.Fatalf("served = %+v", m.served)
	}

	// A listing hands out a link per file: each is reported too.
	e.upload(t, "p1", part{field: recordshttp.FormField, filename: "b.txt", body: "b"})
	m.served = nil
	if files := e.list(t, "p1"); len(files) != 2 || len(m.served) != 2 || m.served[0].Kind != records.ServeLink {
		t.Fatalf("listing %d files reported %+v", len(files), m.served)
	}

	m.err = errors.New("meter down")
	rec := e.do(http.MethodGet, "/files/"+af.FileID, nil, "")
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("Location") != "" {
		t.Fatalf("a read the meter refused answered %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := e.do(http.MethodGet, "/owners/post/p1/gallery", nil, ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("a listing the meter refused answered %d", rec.Code)
	}
}
