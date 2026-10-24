package recordshttp_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records/recordshttp"
)

// envelope is an application's own success shape: the body under "data"
// with the kind of body named, written by the SuccessWriter of
// TestSuccessWriter.
type envelope struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// kindOf names a SuccessBody, through the type switch an application
// writes; the closed interface makes these four every case.
func kindOf(b recordshttp.SuccessBody) string {
	switch b.(type) {
	case recordshttp.ListResponse:
		return "list"
	case recordshttp.AttachedFile:
		return "attachment"
	case recordshttp.FileResponse:
		return "file"
	case recordshttp.StatusResponse:
		return "status"
	}
	return "unknown"
}

func envelopeSuccess(w http.ResponseWriter, _ *http.Request, status int, body recordshttp.SuccessBody) {
	data, err := json.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.app+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope{Kind: kindOf(body), Data: data})
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder, status int, kind string) envelope {
	t.Helper()
	wantStatus(t, rec, status)
	if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.app+json" {
		t.Fatalf("Content-Type = %q: the default writer answered", ct)
	}
	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Kind != kind {
		t.Fatalf("kind = %q, want %q", env.Kind, kind)
	}
	return env
}

// TestSuccessWriter (B5): Config.Respond answers every route with a body
// in the application's envelope, with the status the handler would send
// and the body's type; errors stay with the ErrorWriter.
func TestSuccessWriter(t *testing.T) {
	e := newEnv(t, func(c *recordshttp.Config, _ *[]filesystem.Option) { c.Respond = envelopeSuccess })
	up := decodeEnvelope(t, e.upload(t, "p1", part{"file", "a.txt", "hello"}), http.StatusCreated, "list")
	var list recordshttp.ListResponse
	if err := json.Unmarshal(up.Data, &list); err != nil || len(list.Files) != 1 {
		t.Fatalf("data = %s, %v", up.Data, err)
	}
	id := list.Files[0].FileID
	decodeEnvelope(t, e.do(http.MethodGet, "/owners/post/p1/gallery", nil, ""), http.StatusOK, "list")
	decodeEnvelope(t, e.do(http.MethodGet, "/files/"+id+"/status", nil, ""), http.StatusOK, "status")
	set := fmt.Sprintf(`{"file_id": %q}`, id)
	decodeEnvelope(t, e.do(http.MethodPut, "/owners/post/p2/gallery", strings.NewReader(set), "application/json"), http.StatusOK, "attachment")
	body, ct := form(t, part{"file", "a.txt", "new content"})
	decodeEnvelope(t, e.do(http.MethodPut, "/files/"+id, body, ct), http.StatusOK, "file")
	// An error is not a success: the default error shape answers it.
	wantError(t, e.do(http.MethodGet, "/files/fil_missing", nil, ""), http.StatusNotFound, filesystem.CodeNotFound, "Not Found")
}

// TestDefaultSuccessUnchanged: without Config.Respond the bodies are the
// JSON they always were, byte for byte.
func TestDefaultSuccessUnchanged(t *testing.T) {
	e := newEnv(t)
	up := e.upload(t, "p1", part{"file", "a.txt", "hello"})
	wantStatus(t, up, http.StatusCreated)
	id := decode[recordshttp.ListResponse](t, up).Files[0].FileID
	rec := e.do(http.MethodGet, "/files/"+id+"/status", nil, "")
	want := fmt.Sprintf(`{"file_id":%q,"status":"ready"}`+"\n", id)
	if rec.Body.String() != want || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status body = %q (%s), want %q", rec.Body.String(), rec.Header().Get("Content-Type"), want)
	}
	if !strings.HasPrefix(up.Body.String(), `{"files":[{"attachment_id":"`) || !strings.HasSuffix(up.Body.String(), "}]}\n") {
		t.Fatalf("upload body = %q", up.Body.String())
	}
}
