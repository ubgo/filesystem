package main

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// TestRun walks the whole program against a fresh fake S3 and checks the
// lines the README quotes.
func TestRun(t *testing.T) {
	srv := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
	t.Cleanup(srv.Close)
	var out bytes.Buffer
	if err := run(t.Context(), &out, srv.URL); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		"key field: users/42/${filename}",
		`<input type="hidden" name="Content-Type" value="image/png">`,
		`<input type="hidden" name="Cache-Control" value="public, max-age=86400">`,
		`<input type="file" name="file" accept="image/png">`,
		"browser upload: 200",
		"confirmed: users/42/avatar.png 67 bytes image/png",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
