package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/url"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
)

// parts parses a message and returns its parts in order.
func parts(t *testing.T, raw string) []*multipart.Part {
	t.Helper()
	m, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/mixed" {
		t.Fatalf("content type %q: %v", mt, err)
	}
	r := multipart.NewReader(m.Body, params["boundary"])
	var out []*multipart.Part
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		// Read the body now: NextPart invalidates the previous part.
		body, _ := io.ReadAll(p)
		p.Header.Set("X-Test-Body", string(body))
		out = append(out, p)
	}
}

func TestSend(t *testing.T) {
	srv, err := startSMTP(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	var out bytes.Buffer
	if err := run(t.Context(), &out, srv.Addr().String()); err != nil {
		t.Fatal(err)
	}
	<-srv.done
	ps := parts(t, srv.Last)
	if len(ps) != 2 {
		t.Fatalf("%d parts, want the text and one attachment", len(ps))
	}
	text := ps[0].Header.Get("X-Test-Body")
	i := strings.Index(text, downloadBase)
	if !strings.Contains(text, "all-orders.csv is too large to attach") || i < 0 {
		t.Fatalf("text = %q", text)
	}
	link, err := url.Parse(strings.Fields(text[i:])[0])
	if err != nil {
		t.Fatal(err)
	}
	if link.Query().Get("sig") == "" || link.Query().Get("exp") == "" {
		t.Errorf("link is not signed: %s", link)
	}
	pdf := ps[1]
	if pdf.FileName() != "Facture n°42.pdf" || pdf.Header.Get("Content-Type") != "application/pdf" {
		t.Errorf("attachment = %q %q", pdf.FileName(), pdf.Header.Get("Content-Type"))
	}
	got, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(pdf.Header.Get("X-Test-Body"), "\r\n", ""))
	if err != nil || string(got) != "%PDF-1.7\n% invoice 42\n" {
		t.Errorf("attachment bytes = %q (%v)", got, err)
	}
}

// TestLongAttachment checks that a larger file is encoded in lines of at
// most 76 characters and decodes back to the stored bytes.
func TestLongAttachment(t *testing.T) {
	disk, err := openDisk(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 200_000)
	_, _ = rand.Read(data)
	if _, err := disk.WriteBytes(t.Context(), "scans/contract.bin", data, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	if err := compose(t.Context(), &raw, disk, Message{To: "a@example.org", Subject: "Contract", Files: []File{{Key: "scans/contract.bin", Filename: "contract.bin"}}}); err != nil {
		t.Fatal(err)
	}
	ps := parts(t, raw.String())
	body := ps[1].Header.Get("X-Test-Body")
	for _, line := range strings.Split(strings.TrimRight(body, "\r\n"), "\r\n") {
		if len(line) > base64Line {
			t.Fatalf("line of %d characters", len(line))
		}
	}
	got, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body, "\r\n", ""))
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("round trip failed: %v", err)
	}
}

// TestHeaderInjection checks that a recipient smuggling a header is refused
// before anything is written.
func TestHeaderInjection(t *testing.T) {
	disk, err := openDisk(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	err = compose(t.Context(), &raw, disk, Message{To: "a@example.org\r\nBcc: everyone@example.org", Subject: "x"})
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || raw.Len() != 0 {
		t.Errorf("err = %v, wrote %d bytes", err, raw.Len())
	}
}
