// Command concepts-security runs every security default of the library
// and prints what it refused: traversal keys, types detected from bytes,
// active content, executable file names, mismatched extensions, header
// injection, server-side request forgery, signed URL limits, scanning
// that fails closed, photo metadata stripped by re-encoding, extensions
// from the detected type, the MIME helpers, and secrets kept out of logs.
//
//	go run ./concepts-security          (from the examples directory)
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/scan"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/sniff"
	"github.com/ubgo/filesystem/upload"
)

// Sample contents whose first bytes sniff recognises.
const (
	pngBytes  = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR sample image"
	htmlBytes = "<!DOCTYPE html><script>steal(document.cookie)</script>"
	svgBytes  = `<svg xmlns="http://www.w3.org/2000/svg"><script>steal()</script></svg>`
	// smuggled is a script appended after a real JPEG, which image
	// viewers ignore and a misconfigured server may execute.
	smuggled = "<?php system($_GET['c']); ?>"
	// photoSide is the width and height of the generated photo.
	photoSide = 8
	// maxUpload is the policies' per-file limit: 1 MiB.
	maxUpload = 1 << 20
)

// images is the policy most steps use: images only.
var images = upload.Policy{MaxSize: maxUpload, Allowed: []string{"image/*"}}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is main without the process, so the test can call it.
func run(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("uploads", memory.New())
	if err != nil {
		return err
	}
	traversal(ctx, disk, out)
	detection(out)
	names(out)
	headerInjection(ctx, disk, out)
	ssrf(out)
	signing(ctx, disk, out)
	scanning(ctx, disk, out)
	if err := stripMetadata(ctx, disk, out); err != nil {
		return err
	}
	if err := extensionFromType(ctx, disk, out); err != nil {
		return err
	}
	mimeHelpers(out)
	if err := customDetector(ctx, out); err != nil {
		return err
	}
	return redaction(ctx, out)
}

// traversal: every key is cleaned before a driver sees it, and ".." is
// refused rather than resolved.
// ds:def id=traversal-vtm67qpc
func traversal(ctx context.Context, disk *filesystem.Disk, out io.Writer) {
	_, err := disk.WriteBytes(ctx, "avatars/../../etc/cron.d/job", []byte("x"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "traversal key: %s\n", filesystem.CodeOf(err))
}

// detection: the type comes from the bytes. The name only refines a
// generic answer (plain text, zip) and never into an active type.
// ds:def id=detection-cq5kkx8x
func detection(out io.Writer) {
	for _, c := range []struct{ name, data string }{
		{"invoice.pdf", pngBytes}, {"photo.jpg", htmlBytes}, {"logo.png", svgBytes}, {"page.html", "just text"},
	} {
		t := sniff.Detect([]byte(c.data), c.name)
		fmt.Fprintf(out, "detect %s -> %s (active: %t)\n", c.name, t, sniff.IsActive(t))
	}
	err := images.Accepts(sniff.SVG)
	fmt.Fprintf(out, "policy allowing image/* and an SVG: %s\n", filesystem.CodeOf(err))
}

// names: script and executable extensions are refused in any position of
// the name, and MatchExtension refuses a name that claims another type.
// ds:def id=names-5fjd9yyp
func names(out io.Writer) {
	for _, n := range []string{"shell.php.jpg", ".htaccess", "setup.exe", "holiday.jpg"} {
		fmt.Fprintf(out, "name %s: %q\n", n, filesystem.CodeOf(images.AcceptsName(n, "image/jpeg")))
	}
	strict := images
	strict.MatchExtension = true
	err := strict.AcceptsName("invoice.pdf", "image/png")
	fmt.Fprintf(out, "PNG named invoice.pdf with MatchExtension: %v\n", err)
}

// headerInjection: stored HTTP header values may not contain CR or LF,
// which would let a value add a header of its own when served.
// ds:def id=headerinjection-4se5g2gk
func headerInjection(ctx context.Context, disk *filesystem.Disk, out io.Writer) {
	h := filesystem.HTTPHeaders{ContentDisposition: "attachment\r\nSet-Cookie: session=stolen"}
	_, err := disk.WriteBytes(ctx, "report.pdf", []byte("%PDF-1.7"), filesystem.WriteOptions{Headers: h})
	fmt.Fprintf(out, "header with CRLF: %s\n", filesystem.CodeOf(err))
}

// ssrf: imports refuse every address outside the public internet, checked
// at dial time after DNS, for redirects too.
// ds:def id=ssrf-zsbvsyea
func ssrf(out io.Writer) {
	for _, a := range []string{"127.0.0.1", "10.0.0.8", "169.254.169.254", "100.64.0.1", "::ffff:127.0.0.1", "64:ff9b::a00:1", "fd00::1", "93.184.215.14"} {
		fmt.Fprintf(out, "PublicAddr(%s): %t\n", a, upload.PublicAddr(netip.MustParseAddr(a)))
	}
}

// signing: signed URLs always expire, within seven days, and HMAC keys
// must be at least 32 bytes.
// ds:def id=signing-aaf37gtf
func signing(ctx context.Context, disk *filesystem.Disk, out io.Writer) {
	for _, d := range []time.Duration{0, filesystem.MaxPresignExpiry + time.Second} {
		err := filesystem.ValidatePresign(filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: d})
		fmt.Fprintf(out, "expiry %s: %s\n", d, filesystem.CodeOf(err))
	}
	_, err := signedurl.New("https://app.example.com/files", []signedurl.Key{{ID: "k1", Secret: []byte("short")}})
	fmt.Fprintf(out, "5-byte signing key: %s\n", filesystem.CodeOf(err))
	_, err = disk.SignedURL(ctx, "a.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	fmt.Fprintf(out, "signed URL with no signer configured: %s\n", filesystem.CodeOf(err))
}

// eicar is the industry test signature scanners report for the EICAR
// test file; the demo scanner reports it for any file containing "EICAR".
const eicar = "Win.Test.EICAR_HDB-1"

// demoScanner stands in for clamd: it flags files containing "EICAR".
// down makes it fail like a scanner that is not running.
// ds:def id=demoscanner-79yz8hz2
func demoScanner(down bool) scan.Scanner {
	return scan.ScannerFunc(func(_ context.Context, r io.Reader) (scan.Verdict, error) {
		if down {
			return scan.Verdict{}, errors.New("dial tcp 127.0.0.1:3310: connection refused")
		}
		b, err := io.ReadAll(r)
		if err != nil {
			return scan.Verdict{}, err
		}
		if bytes.Contains(b, []byte("EICAR")) {
			return scan.Verdict{Signature: eicar}, nil
		}
		return scan.Verdict{Clean: true}, nil
	})
}

// scanning: an infected file is refused and deleted, and a scanner that
// is down refuses the upload instead of letting it through unscanned.
// Each case runs on an empty disk, so the count is what the upload left.
// ds:def id=scanning-rsgwpjcn
func scanning(ctx context.Context, _ *filesystem.Disk, out io.Writer) {
	for _, c := range []struct {
		data string
		down bool
	}{{"PNG EICAR", false}, {pngBytes, true}, {pngBytes, false}} {
		disk, err := filesystem.NewDisk("scanned", memory.New())
		if err != nil {
			fmt.Fprintln(out, err)
			return
		}
		p := images
		p.Allowed = []string{"image/*", "text/plain"}
		p.Inspect = scan.Inspect(demoScanner(c.down))
		_, err = upload.Receive(ctx, disk, strings.NewReader(c.data), "file", p)
		fmt.Fprintf(out, "scan (down=%t): %q stored=%d %v\n", c.down, filesystem.CodeOf(err), objects(ctx, disk), err)
	}
}

// objects counts what a disk holds.
func objects(ctx context.Context, disk *filesystem.Disk) int {
	n := 0
	for _, err := range disk.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err == nil {
			n++
		}
	}
	return n
}

// photoWithPayload builds a small JPEG and appends a script after it.
func photoWithPayload() ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, photoSide, photoSide))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		return nil, err
	}
	buf.WriteString(smuggled)
	return buf.Bytes(), nil
}

// stripMetadata: imaging.Sanitize re-encodes photos before they are
// stored, so EXIF, GPS and anything appended after the image are gone.
// ds:def id=stripmetadata-9nbz8sj2
func stripMetadata(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	photo, err := photoWithPayload()
	if err != nil {
		return err
	}
	p := images
	p.Process = imaging.Sanitize(imaging.Options{})
	res, err := upload.Receive(ctx, disk, bytes.NewReader(photo), "holiday.jpg", p)
	if err != nil {
		return err
	}
	stored, err := disk.ReadAll(ctx, res.Key)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "sanitized %s: payload in upload %t, payload stored %t\n",
		res.ContentType, bytes.Contains(photo, []byte(smuggled)), bytes.Contains(stored, []byte(smuggled)))
	return nil
}
