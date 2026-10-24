package share_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/share"
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

// A share link reports every file it hands out, and hands out none it
// could not report.
func TestHandlerReportsServed(t *testing.T) {
	ctx := t.Context()
	m := &meter{}
	disk, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithURLSigner(signer{}))
	mgr, _ := filesystem.NewManager(disk)
	p := share.New()
	s, err := records.New(testdb.Open(t), testdb.Dialect(), mgr, records.WithPlugins(p, m))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = disk.WriteBytes(ctx, "report.pdf", []byte("%PDF-1"), filesystem.WriteOptions{})
	f, err := s.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "report.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	sh, err := p.Create(ctx, s, "t", f.ID, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := share.Handler(s, p)
	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/"+sh.Token, nil))
		return rec
	}
	if rec := get(); rec.Code != http.StatusFound {
		t.Fatalf("GET = %d", rec.Code)
	}
	if len(m.served) != 1 || m.served[0].Kind != records.ServeShare || m.served[0].Subject != sh.ID || m.served[0].FileID != f.ID || m.served[0].Bytes != 6 {
		t.Fatalf("served = %+v", m.served)
	}
	m.err = errors.New("meter down")
	if rec := get(); rec.Code != http.StatusInternalServerError || rec.Header().Get("Location") != "" {
		t.Fatalf("a read the meter refused answered %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}
}
