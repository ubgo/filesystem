package apptest

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/examples/frameworks/internal/app"
)

// InfoKey is the object ExerciseInfo stores on the media disk and reads
// back through the info route.
const InfoKey = "reports/2026/q3.txt"

// SpacedKey has a space, which reaches the router percent-encoded: a
// handler that never unescapes a parameter its router leaves encoded
// looks up "notes/read%20me.txt" and answers 404.
const SpacedKey = "notes/read me.txt"

// EscapeKey holds a literal "%20", which a URL carries as "%2520", and
// PercentKey a lone "%", carried as "%25". Both catch a handler that
// decodes its path parameter twice: unescaping a value the router already
// decoded turns EscapeKey into "notes/50 off.txt" (404) and fails on
// PercentKey's bare "%" (400).
const (
	EscapeKey  = "notes/50%20off.txt"
	PercentKey = "notes/100%.txt"
)

// InfoCase is one request to the info route and what it must answer.
type InfoCase struct {
	Name   string
	Path   string // below app.PathInfo
	User   string
	Status int
	Body   string // a fragment the body must contain
}

// InfoCases are the requests every framework's info handler answers
// alike: the object, a signed-out caller, a missing key, a directory and
// keys that try to leave the disk.
// ds:def id=infocases-9kt9dpsf
var InfoCases = []InfoCase{
	{"object", "/" + InfoKey, "alice", http.StatusOK, `"size":18`},
	{"escaped key", "/notes/read%20me.txt", "alice", http.StatusOK, `"key":"notes/read me.txt"`},
	{"escaped slash", "/notes%2Fread%20me.txt", "alice", http.StatusOK, `"key":"notes/read me.txt"`},
	{"literal %20 in key", "/notes/50%2520off.txt", "alice", http.StatusOK, `"key":"notes/50%20off.txt"`},
	{"literal % in key", "/notes/100%25.txt", "alice", http.StatusOK, `"key":"notes/100%.txt"`},
	{"signed out", "/" + InfoKey, "", http.StatusUnauthorized, `"code":"permission"`},
	{"missing", "/reports/none.txt", "alice", http.StatusNotFound, `{"error":"Not Found","code":"not_found"}`},
	{"directory", "/reports", "alice", http.StatusNotFound, `"code":"not_found"`},
	{"encoded traversal", "/%2E%2E%2Fsecret.txt", "alice", http.StatusBadRequest, `"code":"invalid_key"`},
	{"encoded slash traversal", "/reports%2F..%2F..%2Fsecret.txt", "alice", http.StatusBadRequest, `"code":"invalid_key"`},
}

// ExerciseInfo stores InfoKey, SpacedKey, EscapeKey and PercentKey on a's
// media disk and runs InfoCases through the server at base.
func ExerciseInfo(t *testing.T, base string, a *app.App) {
	t.Helper()
	for _, key := range []string{InfoKey, SpacedKey, EscapeKey, PercentKey} {
		if _, err := a.Disks.Media.WriteBytes(t.Context(), key, []byte(TextBody), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range InfoCases {
		t.Run("info "+c.Name, func(t *testing.T) {
			status, body, _ := Do(t, http.MethodGet, base+app.PathInfo+c.Path, headers(app.HeaderUser, c.User), nil)
			if status != c.Status || !strings.Contains(body, c.Body) {
				t.Errorf("GET %s = %d %s, want %d with %s", c.Path, status, body, c.Status, c.Body)
			}
		})
	}
}
