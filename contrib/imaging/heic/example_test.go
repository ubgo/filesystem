package heic_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/heic"
	"github.com/ubgo/filesystem/upload"
)

// heicPolicy accepts iPhone photos and stores them as upright JPEGs
// without their metadata.
// ds:def id=heicpolicy-6698j5h6
func heicPolicy() upload.Policy {
	p := imaging.NewPipeline(imaging.WithDecoder(heic.Decoder()))
	return upload.Policy{
		MaxSize: 20 << 20,
		Allowed: []string{"image/jpeg", "image/png", heic.ContentType},
		Process: imaging.Sanitize(imaging.Options{}, imaging.WithPipeline(p)),
	}
}

// Example turns a HEIC upload into a JPEG with Sanitize and a Pipeline
// that reads HEIC.
func Example() {
	photo, err := os.ReadFile("testdata/quadrants.heic")
	if err != nil {
		log.Fatal(err)
	}
	r, info, err := heicPolicy().Process(context.Background(), upload.Info{Filename: "IMG_0042.HEIC", ContentType: heic.ContentType}, bytes.NewReader(photo))
	if err != nil {
		log.Fatal(err)
	}
	jpg, _ := io.ReadAll(r)
	fmt.Println(info.Filename, info.ContentType, bytes.HasPrefix(jpg, []byte("\xff\xd8")))
	// Output: IMG_0042.jpg image/jpeg true
}
