package records_test

import (
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// TestSource: the source in the context is recorded, NewFile.Source
// overrides it, and the IP is kept only with WithSourceIP.
func TestSource(t *testing.T) {
	src := records.Source{Channel: "ios", Client: "app/3.2", IP: "203.0.113.9"}
	ctx := records.WithSource(t.Context(), src)

	e := newEnv(t, nil)
	f, err := e.store.Upload(ctx, "t", "media", reader(pngData), "a.png", images)
	if err != nil {
		t.Fatal(err)
	}
	read, _ := e.store.File(ctx, "t", f.ID)
	if read.Source != (records.Source{Channel: "ios", Client: "app/3.2"}) {
		t.Fatalf("without WithSourceIP: %+v", read.Source)
	}

	withIP := newEnv(t, nil, records.WithSourceIP())
	g, err := withIP.store.Upload(ctx, "t", "media", reader(pngData), "a.png", images)
	if err != nil {
		t.Fatal(err)
	}
	if read, _ := withIP.store.File(ctx, "t", g.ID); read.Source != src {
		t.Fatalf("with WithSourceIP: %+v", read.Source)
	}
	if _, err := withIP.disk.WriteBytes(ctx, "b.png", []byte(pngData), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	own := records.Source{Channel: "import"}
	h, err := withIP.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "b.png", Source: own})
	if err != nil || h.Source != own {
		t.Fatalf("NewFile.Source = %+v, %v", h.Source, err)
	}
	if records.SourceFrom(t.Context()) != (records.Source{}) {
		t.Fatal("a source without WithSource")
	}
	long := records.WithSource(t.Context(), records.Source{Client: strings.Repeat("x", records.MaxSourceClientBytes+1)})
	_, err = e.store.Upload(long, "t", "media", reader(pngData), "c.png", images)
	wantCode(t, err, filesystem.CodeInvalidArgument)
}
