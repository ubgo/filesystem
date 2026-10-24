package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/upload"
)

// metadataService is the address cloud instances fetch credentials from,
// the classic target of a server-side request forgery.
var metadataService = netip.MustParseAddr("169.254.169.254")

// fromURL imports a file from a URL. The test server listens on loopback,
// which the SSRF guard refuses like any non-public address; AllowAddr
// lets a known internal server through on purpose.
// ds:def id=fromurl-bxrwf6up
func fromURL(ctx context.Context, out io.Writer) error {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html") // ignored: the bytes decide
		w.Header().Set("Content-Disposition", `attachment; filename="scan.pdf"`)
		_, _ = io.WriteString(w, pdfFile)
	}))
	defer remote.Close()
	disk, err := newDisk()
	if err != nil {
		return err
	}
	_, err = upload.FromURL(ctx, disk, remote.URL+"/files/7", policy, upload.FetchOptions{})
	fmt.Fprintf(out, "from loopback: %v\n", err)
	loopback := func(a netip.Addr) bool { return a.IsLoopback() }
	res, err := upload.FromURL(ctx, disk, remote.URL+"/files/7", policy, upload.FetchOptions{AllowAddr: loopback})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "from allowed server: %q %s %d bytes\n", res.Filename, res.ContentType, res.Size)
	fmt.Fprintf(out, "PublicAddr(%s): %t\n", metadataService, upload.PublicAddr(metadataService))
	_, err = upload.FromURL(ctx, disk, "file:///etc/passwd", policy, upload.FetchOptions{})
	fmt.Fprintf(out, "file URL: %s\n", filesystem.CodeOf(err))
	return nil
}
