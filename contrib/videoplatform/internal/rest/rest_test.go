package rest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
)

type reply struct {
	OK bool `json:"ok"`
}

func noAuth(*http.Request) {}

func TestCall_StatusCodes(t *testing.T) {
	for status, want := range map[int]filesystem.Code{
		http.StatusNotFound:            filesystem.CodeNotFound,
		http.StatusUnauthorized:        filesystem.CodePermission,
		http.StatusForbidden:           filesystem.CodePermission,
		http.StatusTooManyRequests:     filesystem.CodeUnavailable,
		http.StatusBadGateway:          filesystem.CodeUnavailable,
		http.StatusUnprocessableEntity: filesystem.CodeInvalidArgument,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"secret":"echoed"}`, status)
		}))
		err := Call[reply, reply](t.Context(), srv.Client(), "p", http.MethodGet, srv.URL, noAuth, nil, &reply{})
		srv.Close()
		if filesystem.CodeOf(err) != want || strings.Contains(err.Error(), "echoed") {
			t.Errorf("%d: %v, want %s without the body", status, err, want)
		}
	}
}

func TestCall_Bodies(t *testing.T) {
	var gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotType = r.Header.Get("Content-Type")
		switch r.URL.Path {
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat(" ", MaxResponseBytes+1)))
		case "/garbled":
			_, _ = w.Write([]byte("<html>"))
		case "/empty":
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()
	var out reply
	if err := Call(t.Context(), srv.Client(), "p", http.MethodPost, srv.URL, noAuth, &reply{OK: true}, &out); err != nil || !out.OK || gotType != "application/json" {
		t.Fatalf("Call = %+v %q, %v", out, gotType, err)
	}
	if err := Call[reply, reply](t.Context(), srv.Client(), "p", http.MethodGet, srv.URL+"/big", noAuth, nil, &out); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Fatalf("too large: %v", err)
	}
	if err := Call[reply, reply](t.Context(), srv.Client(), "p", http.MethodGet, srv.URL+"/garbled", noAuth, nil, &out); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("garbled: %v", err)
	}
	if err := Call[reply, reply](t.Context(), srv.Client(), "p", http.MethodGet, srv.URL+"/empty", noAuth, nil, &out); err != nil {
		t.Fatalf("204: %v", err)
	}
	if err := Call[reply, reply](t.Context(), srv.Client(), "p", "BAD METHOD", srv.URL, noAuth, nil, nil); err == nil {
		t.Fatal("an invalid method was sent")
	}
	if err := Call[func(), reply](t.Context(), srv.Client(), "p", http.MethodPost, srv.URL, noAuth, &[]func(){nil}[0], nil); err == nil {
		t.Fatal("an unencodable body was sent")
	}
}

// failingBody fails mid-read.
type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, http.ErrHandlerTimeout }
func (failingBody) Close() error             { return nil }

type brokenTransport struct{}

func (brokenTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: failingBody{}}, nil
}

func TestCall_ReadFailure(t *testing.T) {
	client := &http.Client{Transport: brokenTransport{}}
	if err := Call[reply, reply](t.Context(), client, "p", http.MethodGet, "http://x", noAuth, nil, &reply{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("err = %v", err)
	}
}

// TestCall_Unreachable pins that a platform that cannot be reached is
// CodeUnavailable.
func TestCall_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	err := Call[reply, reply](t.Context(), http.DefaultClient, "p", http.MethodGet, srv.URL, noAuth, nil, &reply{})
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("got %v, want CodeUnavailable", err)
	}
}
