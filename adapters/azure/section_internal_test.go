package azure

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestSeekableSection(t *testing.T) {
	// A pipe is an *os.File: it has Seek, but Seek fails.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.WriteString(w, "piped"); _ = w.Close() }()
	rs, n, err := seekableSection(r)
	if err != nil || n != 5 {
		t.Fatalf("pipe = %d, %v", n, err)
	}
	if got, _ := io.ReadAll(rs); string(got) != "piped" {
		t.Fatalf("pipe read %q", got)
	}

	// A seekable body already read from: the section starts where it is,
	// and seeking to 0 (an SDK retry) returns there, not to the file start.
	body := strings.NewReader("skip-part")
	_, _ = body.Seek(5, io.SeekStart)
	rs, n, err = seekableSection(body)
	if err != nil || n != 4 {
		t.Fatalf("section = %d, %v", n, err)
	}
	first, _ := io.ReadAll(rs)
	if pos, err := rs.Seek(0, io.SeekStart); err != nil || pos != 0 {
		t.Fatalf("rewind = %d, %v", pos, err)
	}
	again, _ := io.ReadAll(rs)
	if string(first) != "part" || string(again) != "part" {
		t.Fatalf("read %q then %q", first, again)
	}

	// A body whose position can be read but which cannot seek to its end.
	if _, _, err := seekableSection(halfSeeker{}); !errors.Is(err, errNoEnd) {
		t.Fatalf("half seeker = %v", err)
	}
	if _, _, err := seekableSection(io.MultiReader(iotestErr{})); !errors.Is(err, errRead) {
		t.Fatalf("failing body = %v", err)
	}
}

var (
	errNoEnd = errors.New("cannot seek to the end")
	errRead  = errors.New("read failed")
)

type halfSeeker struct{}

func (halfSeeker) Read([]byte) (int, error) { return 0, io.EOF }
func (halfSeeker) Seek(_ int64, whence int) (int64, error) {
	if whence == io.SeekCurrent {
		return 0, nil
	}
	return 0, errNoEnd
}

type iotestErr struct{}

func (iotestErr) Read([]byte) (int, error) { return 0, errRead }
