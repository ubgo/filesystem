package webhooks

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
)

// redirector answers every request with a 302 to another public URL and
// counts the requests that reach it.
type redirector struct{ n int }

func (rt *redirector) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.n++
	return &http.Response{
		StatusCode: http.StatusFound, Status: "302 Found", Request: r, Body: io.NopCloser(strings.NewReader("")),
		Header: http.Header{"Location": {"https://elsewhere.example/hook"}},
	}, nil
}

// TestDefaultClientFollowsNoRedirect: the default client's redirect
// policy, with the network replaced by an endpoint that answers 302 to a
// public URL. The redirect is not followed, and the delivery records a
// failed attempt.
func TestDefaultClientFollowsNoRedirect(t *testing.T) {
	ctx := t.Context()
	rt := &redirector{}
	c := guardedClient()
	c.Transport = rt
	p, err := New(WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	p.client = c // the guarded client, keeping its redirect policy
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	s, err := records.New(testdb.Open(t), testdb.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AddEndpoint(ctx, s, "t", "https://hooks.example/x"); err != nil {
		t.Fatal(err)
	}
	_, _ = disk.WriteBytes(ctx, "a", []byte("x"), filesystem.WriteOptions{})
	if _, err := s.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "a"}); err != nil {
		t.Fatal(err)
	}
	if n, err := p.Deliver(ctx, s, s.Now().Add(time.Second)); err != nil || n != 0 {
		t.Fatalf("Deliver = %d, %v", n, err)
	}
	d, _ := p.Deliveries(ctx, s, "t", StatePending)
	if rt.n != 1 || len(d) != 1 || d[0].Attempts != 1 || d[0].LastError == "" {
		t.Fatalf("%d requests; deliveries %+v: want one request and a failed attempt", rt.n, d)
	}
}
