package fshttp

import (
	"io"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

func TestRangeSeeker(t *testing.T) {
	ctx := t.Context()
	d, _ := filesystem.NewDisk("d", memory.New())
	_, _ = d.WriteBytes(ctx, "k", []byte("abcdef"), filesystem.WriteOptions{})
	s := &rangeSeeker{ctx: ctx, disk: d, key: "k", size: 6}
	buf := make([]byte, 2)
	if n, _ := s.Read(buf); n != 2 || string(buf) != "ab" {
		t.Fatalf("first read %q", buf[:n])
	}
	if pos, err := s.Seek(1, io.SeekCurrent); err != nil || pos != 3 {
		t.Fatalf("SeekCurrent = %d, %v", pos, err)
	}
	if n, _ := s.Read(buf); string(buf[:n]) != "de" {
		t.Errorf("after seek %q", buf[:n])
	}
	if pos, _ := s.Seek(0, io.SeekEnd); pos != 6 {
		t.Errorf("SeekEnd = %d", pos)
	}
	if n, err := s.Read(buf); n != 0 || err != io.EOF {
		t.Errorf("read at end = %d, %v", n, err)
	}
	if _, err := s.Seek(0, 42); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("bad whence: %v", err)
	}
	if _, err := s.Seek(-7, io.SeekEnd); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("negative offset: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Error(err)
	}
}
