package upload_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

func TestAcceptsName(t *testing.T) {
	p := upload.Policy{MaxSize: 1, Allowed: []string{"*/*"}, Extensions: []string{".png", ".JPG", ".pdf"}, MatchExtension: true}
	for _, c := range []struct {
		name, contentType string
		ok                bool
	}{
		{"a.png", "image/png", true},
		{"A.PNG", "image/png", true},
		{"photo.jpg", "image/jpeg", true},   // allowed in another case
		{"photo", "image/jpeg", true},       // no extension: the type check decides
		{"../../x.png", "image/png", true},  // the base name is what counts
		{"invoice.pdf", "image/png", false}, // the name lies about the content
		{"a.gif", "image/gif", false},       // not in the list
		{"a.exe", "application/x-msdownload", false},
	} {
		err := p.AcceptsName(c.name, c.contentType)
		if (err == nil) != c.ok || (err != nil && filesystem.CodeOf(err) != filesystem.CodeRejected) {
			t.Errorf("AcceptsName(%q, %q) = %v", c.name, c.contentType, err)
		}
	}
	// Without the list, an unknown extension passes MatchExtension.
	loose := upload.Policy{MaxSize: 1, Allowed: []string{"*/*"}, MatchExtension: true}
	if err := loose.AcceptsName("data.xyz", "image/png"); err != nil {
		t.Errorf("unknown extension = %v", err)
	}
	if err := (upload.Policy{MaxSize: 1, Allowed: []string{"*/*"}}).AcceptsName("a.pdf", "image/png"); err != nil {
		t.Errorf("mismatch without MatchExtension = %v", err)
	}
	for _, bad := range []string{"png", ".", "", "./x", ".a/b"} {
		if err := (upload.Policy{MaxSize: 1, Allowed: []string{"*/*"}, Extensions: []string{bad}}).Validate(); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("extension %q accepted: %v", bad, err)
		}
	}
}

// TestNameRulesOnEveryPath checks each upload path applies the rules.
func TestNameRulesOnEveryPath(t *testing.T) {
	ctx := t.Context()
	p := images
	p.MatchExtension = true

	disk := newDisk(t, memory.New())
	_, err := upload.Receive(ctx, disk, strings.NewReader(pngData), "invoice.pdf", p)
	wantCode(t, err, filesystem.CodeRejected)
	if n := countKeys(t, disk); n != 0 {
		t.Fatalf("%d objects after a refused name", n)
	}

	signing := newDisk(t, presigning{memory.New()})
	_, err = upload.Presign(ctx, signing, upload.TicketRequest{Filename: "a.gif", ContentType: "image/png", Size: 10}, p, time.Minute)
	wantCode(t, err, filesystem.CodeRejected)
	ticket, err := upload.Presign(ctx, signing, upload.TicketRequest{Filename: "a.gif", ContentType: "image/gif", Size: 10}, p, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = signing.WriteBytes(ctx, ticket.Key, []byte(pngData), filesystem.WriteOptions{}) // a PNG behind a GIF ticket
	_, err = upload.Confirm(ctx, signing, ticket.Key, "a.gif", p)
	wantCode(t, err, filesystem.CodeRejected)
	if ok, _ := signing.Exists(ctx, ticket.Key); ok {
		t.Fatal("a refused object was kept")
	}

	c := uppyClient{t, upload.Uppy(signing, p, upload.UppyOptions{})}
	if rec := c.call(http.MethodPost, "/multipart", `{"filename":"x.pdf","type":"image/png"}`); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("uppy create with a lying name = %d", rec.Code)
	}
}

func TestInspectors(t *testing.T) {
	var order []string
	step := func(name string, err error) upload.Inspector {
		return func(context.Context, *filesystem.Disk, upload.Result) error { order = append(order, name); return err }
	}
	boom := errors.New("refused")
	err := upload.Inspectors(step("scan", nil), nil, step("decode", boom), step("moderate", nil))(t.Context(), nil, upload.Result{})
	if !errors.Is(err, boom) || strings.Join(order, ",") != "scan,decode" {
		t.Fatalf("err %v, ran %v", err, order)
	}
	if err := upload.Inspectors()(t.Context(), nil, upload.Result{}); err != nil {
		t.Fatal(err)
	}
}

func TestXMLIsActiveContent(t *testing.T) {
	p := upload.Policy{MaxSize: 1, Allowed: []string{"*/*"}}
	if err := p.Accepts("text/xml"); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("XML by default = %v", err)
	}
	p.AllowActiveContent = true
	if err := p.Accepts("text/xml"); err != nil {
		t.Fatalf("XML when allowed = %v", err)
	}
}

func TestExecutableNamesAreRefused(t *testing.T) {
	p := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}}
	for _, name := range []string{"shell.php", "SHELL.PHP", "shell.php.jpg", "photo.jpg.exe", ".htaccess", "run.sh", "x.PHTML", "../../a.aspx"} {
		if err := p.AcceptsName(name, "text/plain"); filesystem.CodeOf(err) != filesystem.CodeRejected {
			t.Errorf("%q accepted: %v", name, err)
		}
	}
	for _, name := range []string{"report.pdf", "photo.jpg", "notes", "php-tips.txt", "my.shell.notes.txt"} {
		if err := p.AcceptsName(name, "text/plain"); err != nil {
			t.Errorf("%q refused: %v", name, err)
		}
	}
	p.AllowActiveContent = true
	if err := p.AcceptsName("main.py", "text/plain"); err != nil {
		t.Errorf("a code host's script = %v", err)
	}
	// The refusal applies on the upload path, before anything is stored.
	disk := newDisk(t, memory.New())
	_, err := upload.Receive(t.Context(), disk, strings.NewReader("<?php system($_GET['c']); ?>"), "avatar.php.png", upload.Policy{MaxSize: 1 << 10, Allowed: []string{"text/*"}})
	wantCode(t, err, filesystem.CodeRejected)
	if n := countKeys(t, disk); n != 0 {
		t.Fatalf("%d objects", n)
	}
}
