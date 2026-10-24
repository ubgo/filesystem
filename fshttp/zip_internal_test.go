package fshttp

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// failAfter accepts n bytes, then fails every write, as a connection the
// client closed does.
type failAfter struct{ n int }

var errGone = errors.New("client gone")

func (f *failAfter) Write(p []byte) (int, error) {
	if len(p) > f.n {
		return 0, errGone
	}
	f.n -= len(p)
	return len(p), nil
}

// TestWriteZipStopsWhenTheEntryHeaderCannotBeWritten covers the failure of
// the next entry's header, which a client hanging up between two entries
// causes.
func TestWriteZipStopsWhenTheEntryHeaderCannotBeWritten(t *testing.T) {
	d, _ := filesystem.NewDisk("media", memory.New())
	// zip.Writer buffers 4096 bytes. The first entry (a 35-byte local
	// header and 4040 bytes) just fits, so nothing is flushed until the
	// second CreateHeader adds the data descriptor and the next header.
	e, _ := d.WriteBytes(t.Context(), "a.txt", bytes.Repeat([]byte("a"), 4040), filesystem.WriteOptions{})
	entries := []zipEntry{{name: "a.txt", e: e}, {name: "b.txt", e: e}}
	err := writeZip(t.Context(), &failAfter{n: 0}, d, entries, false)
	if !errors.Is(err, errGone) {
		t.Errorf("writeZip = %v, want the writer's error", err)
	}
}
