package variants_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
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

// The handler reports every variant it hands out, with the variant's own
// size, and hands out none it could not report.
func TestHandlerReportsServed(t *testing.T) {
	db, _ := recordstest.Open(t)
	disk, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithURLSigner(signer{}))
	mgr, _ := filesystem.NewManager(disk)
	p, err := variants.New(transforms, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := &meter{}
	s, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(p, m))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	e := &env{db: db, disk: disk, store: s, plugin: p}
	original := pngBytes(t, 600, 400)
	f := e.image(t, "a.png", original, filesystem.VisibilityPrivate)
	h := variants.Handler(s, p, func(*http.Request) (string, error) { return "t", nil }, variants.HandlerOptions{})
	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/"+f.ID+"/thumb", nil))
		return rec
	}
	if rec := get(); rec.Code != http.StatusFound {
		t.Fatalf("GET = %d", rec.Code)
	}
	if len(m.served) != 1 || m.served[0].Kind != records.ServeVariant || m.served[0].Subject != "thumb" || m.served[0].FileID != f.ID || m.served[0].Scope != "t" ||
		m.served[0].Bytes <= 0 || m.served[0].Bytes == int64(len(original)) {
		t.Fatalf("served = %+v; want the thumb's own size", m.served)
	}
	// SrcSet and URL issue links too: each is reported.
	m.served = nil
	if _, list, err := p.SrcSet(t.Context(), s, "t", f.ID, time.Minute, "thumb"); err != nil || len(list) != 1 || len(m.served) != 1 || m.served[0].Kind != records.ServeVariant || m.served[0].Subject != "thumb" {
		t.Fatalf("SrcSet = %v, %v; served %+v", list, err, m.served)
	}
	if _, err := p.URL(t.Context(), s, "t", f.ID, "thumb", time.Minute); err != nil || len(m.served) != 2 {
		t.Fatalf("URL = %v; served %+v", err, m.served)
	}

	m.err = errors.New("meter down")
	if rec := get(); rec.Code != http.StatusInternalServerError || rec.Header().Get("Location") != "" {
		t.Fatalf("a read the meter refused answered %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}
}
