package upload_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
)

// headerless is a memory driver that does not store HTTP headers.
type headerless struct{ *memory.Driver }

func (h headerless) Capabilities() filesystem.Capabilities {
	c := h.Driver.Capabilities()
	c.Headers = false
	return c
}

const immutable = "public, max-age=31536000, immutable"

func TestReceiveStoresPolicyHeaders(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, memory.New())
	p := images
	p.Headers = upload.Attachment(immutable)
	for name, want := range map[string]string{
		"photo.png":    `attachment; filename=photo.png`,
		`my "cat".png`: `attachment; filename="my \"cat\".png"`,
		"fête.png":     `attachment; filename*=utf-8''f%C3%AAte.png`,
	} {
		res, err := upload.Receive(ctx, d, strings.NewReader(pngData), name, p)
		if err != nil {
			t.Fatal(err)
		}
		e, err := d.Stat(ctx, res.Key)
		if err != nil {
			t.Fatal(err)
		}
		if e.Headers.ContentDisposition != want || e.Headers.CacheControl != immutable {
			t.Errorf("%q stored %+v, want disposition %s", name, e.Headers, want)
		}
	}

	// The headers see the file as stored, after Process renamed it.
	p.Process = func(_ context.Context, in upload.Info, body io.Reader) (io.Reader, upload.Info, error) {
		return body, upload.Info{Filename: "clean.png"}, nil
	}
	res, err := upload.Receive(ctx, d, strings.NewReader(pngData), "raw.png", p)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := d.Stat(ctx, res.Key); e.Headers.ContentDisposition != "attachment; filename=clean.png" {
		t.Errorf("after Process: %+v", e.Headers)
	}
}

func TestPolicyHeadersRefusals(t *testing.T) {
	ctx := t.Context()
	// A disk that cannot store them refuses rather than dropping them.
	plain := newDisk(t, headerless{memory.New()})
	p := images
	p.Headers = upload.StaticHeaders(filesystem.HTTPHeaders{CacheControl: immutable})
	_, err := upload.Receive(ctx, plain, strings.NewReader(pngData), "a.png", p)
	wantCode(t, err, filesystem.CodeUnsupported)
	if n := count(t, plain); n != 0 {
		t.Errorf("%d objects stored", n)
	}
	// A header value that would split the line is refused.
	d := newDisk(t, memory.New())
	p.Headers = upload.StaticHeaders(filesystem.HTTPHeaders{CacheControl: "x\r\nSet-Cookie: a=b"})
	_, err = upload.Receive(ctx, d, strings.NewReader(pngData), "a.png", p)
	wantCode(t, err, filesystem.CodeInvalidArgument)
	// The HeadersFunc's own error refuses the upload.
	p.Headers = func(upload.Info) (filesystem.HTTPHeaders, error) {
		return filesystem.HTTPHeaders{}, filesystem.Errorf(filesystem.CodeRejected, "no")
	}
	_, err = upload.Receive(ctx, d, strings.NewReader(pngData), "a.png", p)
	wantCode(t, err, filesystem.CodeRejected)
	// A presigning driver that cannot pin headers refuses instead of
	// losing them; a policy whose function stores none still presigns.
	pd := newDisk(t, presigning{memory.New()})
	p.Headers = upload.Attachment("")
	_, err = upload.Presign(ctx, pd, upload.TicketRequest{Filename: "a.png", ContentType: "image/png", Size: 10}, p, time.Minute)
	wantCode(t, err, filesystem.CodeUnsupported)
	p.Headers = upload.StaticHeaders(filesystem.HTTPHeaders{})
	if _, err := upload.Presign(ctx, pd, upload.TicketRequest{Filename: "a.png", ContentType: "image/png", Size: 10}, p, time.Minute); err != nil {
		t.Errorf("empty headers: %v", err)
	}
	if n := count(t, d); n != 0 {
		t.Errorf("%d objects stored by refused uploads", n)
	}
}

func TestUppyStoresPolicyHeaders(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, presigning{memory.New()})
	p := images
	p.Headers = upload.Attachment(immutable)
	c := uppyClient{t, upload.Uppy(d, p, upload.UppyOptions{})}
	rec := c.call(http.MethodPost, "/multipart", `{"filename":"big.png","type":"image/png"}`)
	var created struct{ UploadID, Key string }
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &created) != nil {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	part, err := d.UploadPart(ctx, created.Key, created.UploadID, 1, reader(pngData), int64(len(pngData)))
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"parts":[{"PartNumber":1,"ETag":"\"%s\""}]}`, part.ETag)
	if rec := c.call(http.MethodPost, "/multipart/"+created.UploadID+"/complete?key="+created.Key, body); rec.Code != 200 {
		t.Fatalf("complete = %d %s", rec.Code, rec.Body)
	}
	e, err := d.Stat(ctx, created.Key)
	if err != nil {
		t.Fatal(err)
	}
	if e.Headers.CacheControl != immutable || e.Headers.ContentDisposition != "attachment; filename=big.png" {
		t.Errorf("stored %+v", e.Headers)
	}
}

func TestHeadersFuncErrorsRefuseEveryPath(t *testing.T) {
	ctx := t.Context()
	p := images
	p.Headers = func(upload.Info) (filesystem.HTTPHeaders, error) {
		return filesystem.HTTPHeaders{}, filesystem.Errorf(filesystem.CodeRejected, "no headers for you")
	}
	d := newDisk(t, presigning{memory.New()})
	_, err := upload.Presign(ctx, d, upload.TicketRequest{Filename: "a.png", ContentType: "image/png", Size: 10}, p, time.Minute)
	wantCode(t, err, filesystem.CodeRejected)
	c := uppyClient{t, upload.Uppy(d, p, upload.UppyOptions{})}
	if rec := c.call(http.MethodPost, "/multipart", `{"filename":"big.png","type":"image/png"}`); rec.Code != http.StatusUnsupportedMediaType || !strings.Contains(rec.Body.String(), "no headers for you") {
		t.Errorf("uppy create = %d %s", rec.Code, rec.Body)
	}
	// Odd names still make a disposition: control bytes and invalid UTF-8
	// are encoded, never dropped.
	h, err := upload.Attachment("")(upload.Info{Filename: "a\x00\xff.png"})
	if err != nil || h.ContentDisposition != "attachment; filename*=utf-8''a%00%FF.png" {
		t.Errorf("odd name: %q, %v", h.ContentDisposition, err)
	}
}

func TestPresignPinsPolicyHeaders(t *testing.T) {
	ctx := t.Context()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	signer, err := signedurl.New(srv.URL+"/files", []signedurl.Key{{ID: "k1", Secret: bytes.Repeat([]byte("s"), 32)}})
	if err != nil {
		t.Fatal(err)
	}
	d := newDisk(t, memory.New(), filesystem.WithURLSigner(signer))
	m, _ := filesystem.NewManager(d)
	mux.Handle("/files/", http.StripPrefix("/files", fshttp.Signed(m, signer)))

	p := images
	p.Headers = upload.Attachment(immutable)
	tk, err := upload.Presign(ctx, d, upload.TicketRequest{Filename: "me.png", ContentType: "image/png", Size: int64(len(pngData))}, p, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if h := filesystem.HTTPHeadersFrom(tk.Headers); h.CacheControl != immutable || h.ContentDisposition != "attachment; filename=me.png" {
		t.Fatalf("ticket headers %v", tk.Headers)
	}
	// The client sends exactly what the ticket lists.
	req, _ := http.NewRequestWithContext(ctx, tk.Method, tk.URL, strings.NewReader(pngData))
	req.Header = tk.Headers
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	if _, err := upload.Confirm(ctx, d, tk.Key, "me.png", p); err != nil {
		t.Fatal(err)
	}
	if e, _ := d.Stat(ctx, tk.Key); e.Headers.ContentDisposition != "attachment; filename=me.png" || e.Headers.CacheControl != immutable {
		t.Errorf("stored %+v", e.Headers)
	}

	// A disk whose driver stores no headers refuses the ticket.
	plain := newDisk(t, headerless{memory.New()}, filesystem.WithURLSigner(signer))
	_, err = upload.Presign(ctx, plain, upload.TicketRequest{Filename: "me.png", ContentType: "image/png", Size: 10}, p, time.Minute)
	wantCode(t, err, filesystem.CodeUnsupported)
}

// The Attachment doc's three forms are what it produces.
func TestAttachmentForms(t *testing.T) {
	for name, want := range map[string]string{
		"report.pdf":    "attachment; filename=report.pdf",
		"my report.pdf": `attachment; filename="my report.pdf"`,
		"fête.png":      "attachment; filename*=utf-8''f%C3%AAte.png",
	} {
		if h, _ := upload.Attachment("")(upload.Info{Filename: name}); h.ContentDisposition != want {
			t.Errorf("%q: %q, want %q", name, h.ContentDisposition, want)
		}
	}
}
