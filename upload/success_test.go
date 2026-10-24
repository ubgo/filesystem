package upload_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// envelope is an application's own success shape, {"data": ..., "kind": ...}.
type envelope struct {
	Data upload.SuccessBody `json:"data"`
	Kind string             `json:"kind"`
}

// enveloped is a SuccessWriter that wraps every body in envelope and
// answers 200, naming the body's type so the closed set is exercised.
func enveloped(w http.ResponseWriter, _ *http.Request, _ int, body upload.SuccessBody) {
	kind := ""
	switch body.(type) {
	case upload.Response:
		kind = "files"
	case upload.Ticket:
		kind = "ticket"
	case upload.Limits:
		kind = "limits"
	case upload.ChunkProgress:
		kind = "progress"
	}
	w.Header().Set("Content-Type", "application/vnd.app+json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(envelope{Data: body, Kind: kind})
}

// kindOf decodes an envelope's kind.
func kindOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return e.Kind
}

// TestSuccessWriterIsUsed proves every handler with the hook answers its
// success through it: the custom status, media type and envelope.
func TestSuccessWriterIsUsed(t *testing.T) {
	d := newDisk(t, presigning{memory.New()})
	body, ct := multipartBody(t, []formFile{{"file", "a.png", pngData}}, nil)
	rec := post(t, upload.Handler(d, images, upload.HandlerOptions{Respond: enveloped}), body, ct)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/vnd.app+json" || kindOf(t, rec) != "files" {
		t.Errorf("Handler: %d %s %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	ticket, _ := json.Marshal(upload.TicketRequest{Filename: "a.png", ContentType: "image/png", Size: 10})
	rec = post(t, upload.PresignHandlerWith(d, images, time.Minute, upload.PresignHandlerOptions{Respond: enveloped}), bytes.NewReader(ticket), "application/json")
	if rec.Code != http.StatusOK || kindOf(t, rec) != "ticket" {
		t.Errorf("PresignHandler: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	upload.LimitsHandler(images, upload.LimitsOptions{Respond: enveloped}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || kindOf(t, rec) != "limits" {
		t.Errorf("LimitsHandler: %d %s", rec.Code, rec.Body)
	}
}

// TestSuccessDefaultUnchanged proves a nil hook answers exactly what the
// handlers answered before it existed: the status and the JSON encoding
// of the body type, byte for byte.
func TestSuccessDefaultUnchanged(t *testing.T) {
	d := newDisk(t, presigning{memory.New()})
	body, ct := multipartBody(t, []formFile{{"file", "a.png", pngData}}, nil)
	rec := post(t, upload.Handler(d, images, upload.HandlerOptions{}), body, ct)
	var res upload.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusCreated || rec.Header().Get("Content-Type") != "application/json" || rec.Body.String() != encoded(t, res) {
		t.Errorf("Handler default: %d %s %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	rec = httptest.NewRecorder()
	upload.LimitsHandler(images, upload.LimitsOptions{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != encoded(t, upload.LimitsOf(images, upload.LimitsOptions{})) {
		t.Errorf("Limits default: %d %q", rec.Code, rec.Body)
	}
	ticket, _ := json.Marshal(upload.TicketRequest{Filename: "a.png", ContentType: "image/png", Size: 10})
	rec = post(t, upload.PresignHandler(d, images, time.Minute), bytes.NewReader(ticket), "application/json")
	var tk upload.Ticket
	if err := json.Unmarshal(rec.Body.Bytes(), &tk); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || rec.Body.String() != encoded(t, tk) || tk.Key == "" {
		t.Errorf("PresignHandler default: %d %q", rec.Code, rec.Body)
	}
	// A Partial request that stored nothing still answers its Response
	// with the failure's status through the default.
	body, ct = multipartBody(t, []formFile{{"file", "a.html", htmlData}}, nil)
	rec = post(t, upload.Handler(d, images, upload.HandlerOptions{Partial: true}), body, ct)
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || rec.Code < http.StatusBadRequest || len(res.Failed) != 1 || res.Failed[0].Code != filesystem.CodeRejected {
		t.Errorf("Partial default: %d %q %v", rec.Code, rec.Body, err)
	}
}

// encoded is v as json.Encoder writes it (with the trailing newline).
func encoded[T upload.Response | upload.Limits | upload.Ticket](t *testing.T, v T) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.NewEncoder(&b).Encode(v); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
