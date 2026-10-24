// Command svg uploads an SVG the way an application that accepts them
// would with contrib/svg: svg.Policy lets SVG through an image policy and
// nothing else that browsers run, sanitises it before it is stored, and
// refuses one that would still carry active content. It prints what the
// sanitiser removed and the document that was stored, then draws the
// stored document as a PNG thumbnail with contrib/svg/raster.
//
//	go run ./svg logo.svg      (from examples/media)
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/svg"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// thumbSide is the box the raster thumbnail of a stored SVG fits in.
const thumbSide = 128

// images is the application's image policy before SVG is let in.
var images = upload.Policy{
	MaxSize: 2 << 20,
	Allowed: []string{"image/png", "image/jpeg", "image/webp", svg.ContentType},
	Key:     upload.RandomKey("logos"),
}

// newPolicy is images with SVG accepted safely: sanitised on the way in,
// refused if still active at Inspect, every other active type denied.
// ds:def id=newpolicy-8uk22a4e
func newPolicy() (upload.Policy, error) {
	return svg.Policy(images, svg.Options{AllowDataImages: true})
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: svg <file>")
	}
	body, err := os.ReadFile(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	if err := run(context.Background(), os.Stdout, body, os.Args[1]); err != nil {
		log.Fatal(err)
	}
}

// run reports what sanitising removes from body, then uploads it under
// the SVG-aware policy and prints the stored document.
// ds:def id=run-3mvatpcw
func run(ctx context.Context, out io.Writer, body []byte, filename string) error {
	rep, err := svg.Sanitize(bytes.NewReader(body), io.Discard, svg.Options{AllowDataImages: true})
	if err != nil {
		return err
	}
	for _, r := range rep.Removed {
		fmt.Fprintf(out, "removed   %s\n", r)
	}
	p, err := newPolicy()
	if err != nil {
		return err
	}
	disk, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		return err
	}
	res, err := upload.Receive(ctx, disk, bytes.NewReader(body), filename, p)
	if err != nil {
		return err
	}
	stored, err := disk.ReadAll(ctx, res.Key)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stored    %s (%s, %d bytes)\n%s\n", res.Key, res.ContentType, res.Size, stored)
	png, err := thumbnail(stored, thumbSide)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "raster    %dx%d %s\n", png.Width, png.Height, png.ContentType)
	return nil
}
