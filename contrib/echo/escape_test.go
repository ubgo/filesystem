package fsecho_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/ubgo/filesystem"
	fsecho "github.com/ubgo/filesystem/contrib/echo"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
)

// escapedKey holds a space, a literal "%20" and a non-ASCII character, the
// three things a handler reading a still percent-encoded path gets wrong.
const escapedKey = "notes/read me %20 é.txt"

// TestMount_DecodesEscapedKeys pins that a library handler behind Mount
// sees the decoded key, however the client escaped it. echo's own c.Param
// hands a wildcard over raw whenever the request path holds a
// non-canonical escape (such as %2F), so Mount must not route keys
// through it; it strips the prefix from the request's decoded path. An
// encoded traversal still reaches the Disk only as a key, which refuses it.
func TestMount_DecodesEscapedKeys(t *testing.T) {
	disk, err := filesystem.NewDisk("media", memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := disk.WriteBytes(t.Context(), escapedKey, []byte("hello"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := disk.WriteBytes(t.Context(), "secret.txt", []byte("no"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	fsecho.Mount(e.Group("/files"), "/public", fshttp.Public(disk))
	for _, c := range []struct {
		name, path string
		status     int
	}{
		{"canonical escapes", "/files/public/notes/read%20me%20%2520%20%C3%A9.txt", http.StatusOK},
		{"escaped slash", "/files/public/notes%2Fread%20me%20%2520%20%C3%A9.txt", http.StatusOK},
		{"encoded traversal", "/files/public/notes/%2E%2E%2F%2E%2E%2Fsecret.txt", http.StatusBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
			body, _ := io.ReadAll(rec.Body)
			if rec.Code != c.status || (c.status == http.StatusOK && string(body) != "hello") {
				t.Fatalf("GET %s = %d %q, want %d", c.path, rec.Code, body, c.status)
			}
		})
	}
}
