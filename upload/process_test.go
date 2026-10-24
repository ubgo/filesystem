package upload_test

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// upper is a processor that upper-cases text and renames it.
func upper(_ context.Context, in upload.Info, body io.Reader) (io.Reader, upload.Info, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, upload.Info{}, err
	}
	return strings.NewReader(strings.ToUpper(string(data))), upload.Info{Filename: "UPPER-" + in.Filename}, nil
}

func TestProcess(t *testing.T) {
	ctx := t.Context()
	disk := newDisk(t, memory.New())
	var inspected upload.Result
	p := upload.Policy{MaxSize: 32, Allowed: []string{"text/plain", "image/png"}, Process: upload.Processor(upper),
		Inspect: func(_ context.Context, _ *filesystem.Disk, r upload.Result) error { inspected = r; return nil }}

	res, err := upload.Receive(ctx, disk, strings.NewReader("hello"), "a.txt", p)
	if err != nil || res.Filename != "UPPER-a.txt" || res.Size != 5 || !reflect.DeepEqual(inspected, res) {
		t.Fatalf("processed = %+v, %v", res, err)
	}
	if got, _ := disk.ReadAll(ctx, res.Key); string(got) != "HELLO" {
		t.Fatalf("stored %q", got)
	}

	cases := map[string]struct {
		process upload.Processor
		code    filesystem.Code
	}{
		"refused output type": {func(_ context.Context, in upload.Info, b io.Reader) (io.Reader, upload.Info, error) {
			return b, upload.Info{ContentType: "text/html"}, nil
		}, filesystem.CodeRejected},
		"processor error": {func(context.Context, upload.Info, io.Reader) (io.Reader, upload.Info, error) {
			return nil, upload.Info{}, filesystem.Errorf(filesystem.CodeRejected, "not a real photo")
		}, filesystem.CodeRejected},
		"output too large": {func(_ context.Context, in upload.Info, _ io.Reader) (io.Reader, upload.Info, error) {
			return strings.NewReader(strings.Repeat("x", 64)), in, nil
		}, filesystem.CodeTooLarge},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			q := p
			q.Process = c.process
			_, err := upload.Receive(ctx, disk, strings.NewReader("hello"), "a.txt", q)
			if filesystem.CodeOf(err) != c.code {
				t.Fatalf("err = %v, want %s", err, c.code)
			}
		})
	}
	if n := countKeys(t, disk); n != 1 {
		t.Fatalf("%d objects, want only the processed one", n)
	}

	// A processor keeping the name and type leaves both as detected.
	q := p
	q.Process = func(_ context.Context, _ upload.Info, b io.Reader) (io.Reader, upload.Info, error) {
		return b, upload.Info{}, nil
	}
	if res, err := upload.Receive(ctx, disk, strings.NewReader("hi"), "b.txt", q); err != nil || res.Filename != "b.txt" || !strings.HasPrefix(res.ContentType, "text/plain") {
		t.Fatalf("pass-through = %+v, %v", res, err)
	}
}
