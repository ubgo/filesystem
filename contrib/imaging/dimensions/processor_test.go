package dimensions_test

import (
	"io"
	"testing"

	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/dimensions"
	"github.com/ubgo/filesystem/records"
)

// heicStandIn reads any header as a 4032x3024 HEIC, standing in for the
// HEIC codec module.
type heicStandIn struct{ imaging.Processor }

func (heicStandIn) Probe(r io.Reader) (imaging.Info, error) {
	_, _ = io.Copy(io.Discard, r)
	return imaging.Info{Format: "heic", Width: 4032, Height: 3024}, nil
}

// TestWithProcessor pins that an added processor and content type are
// used at create and by the backfill, and that the built-in types still
// go through it.
func TestWithProcessor(t *testing.T) {
	opt := dimensions.WithProcessor(heicStandIn{imaging.Default()}, "image/heic")
	e := newEnv(t, opt)
	f, err := e.record(t, "IMG_0001.heic", []byte("ftypheic..."), records.NewFile{ContentType: "image/heic"})
	if err != nil || dims(f) != [3]int{4032, 3024, 1} {
		t.Fatalf("HEIC = %v, %v", f.Metadata, err)
	}
	// Without the option the type is not measured.
	plain := newEnv(t)
	g, err := plain.record(t, "IMG_0002.heic", []byte("ftypheic..."), records.NewFile{ContentType: "image/heic"})
	if err != nil || dims(g)[2] != 0 {
		t.Errorf("HEIC without the processor = %v, %v", g.Metadata, err)
	}
	// The backfill uses the installed plugin's processor and types.
	res, err := dimensions.Backfill(t.Context(), plain.store, dimensions.BackfillOptions{Scope: "t", Plugin: dimensions.New(opt)})
	if err != nil || res.Measured != 1 {
		t.Errorf("Backfill with the plugin = %+v, %v", res, err)
	}
}
