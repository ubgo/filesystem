package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/upload"
)

// loopback allows exactly the address httptest listens on.
var loopback = func(a netip.Addr) bool { return a == netip.MustParseAddr("127.0.0.1") }

// setup serves the app with the given AllowAddr; the samples routes on the
// same server stand in for a remote site.
func setup(t *testing.T, allow func(netip.Addr) bool) *App {
	t.Helper()
	app, err := New(t.Context(), t.TempDir(), fetchOptions(allow))
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func startServer(t *testing.T, app *App) string {
	t.Helper()
	srv := httptest.NewServer(app.Handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

// post imports url as u1 and returns the status and decoded body.
func post(t *testing.T, base, url string) (int, fileResponse, upload.ErrorResponse) {
	t.Helper()
	body, _ := json.Marshal(importRequest{URL: url})
	req, _ := http.NewRequest(http.MethodPost, base+"/imports", strings.NewReader(string(body)))
	req.Header.Set(headerUser, "u1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var f fileResponse
	var e upload.ErrorResponse
	if resp.StatusCode == http.StatusCreated {
		_ = json.NewDecoder(resp.Body).Decode(&f)
	} else {
		_ = json.NewDecoder(resp.Body).Decode(&e)
	}
	return resp.StatusCode, f, e
}

func TestImportFromAllowedHost(t *testing.T) {
	app := setup(t, loopback)
	base := startServer(t, app)
	code, f, _ := post(t, base, base+"/samples/photo.jpg")
	if code != http.StatusCreated || f.Filename != "sunset.jpg" || f.ContentType != "image/jpeg" {
		t.Fatalf("import = %d %+v", code, f)
	}
	cases := map[string]int{
		base + "/samples/huge.jpg":    http.StatusRequestEntityTooLarge,
		base + "/samples/metadata":    http.StatusBadRequest, // the redirect is checked too
		base + "/samples/missing.jpg": http.StatusNotFound,
	}
	for url, want := range cases {
		if code, _, e := post(t, base, url); code != want {
			t.Errorf("%s = %d %+v, want %d", url, code, e, want)
		}
	}
}

func TestGuardRefusesInternalAddresses(t *testing.T) {
	app := setup(t, nil)
	base := startServer(t, app)
	for _, url := range []string{
		base + "/samples/photo.jpg",
		"http://10.0.0.5/admin",
		"http://169.254.169.254/latest/meta-data/",
		"file:///etc/passwd",
		"http://user:pw@example.com/a.jpg",
	} {
		if code, _, e := post(t, base, url); code != http.StatusBadRequest || e.Code != filesystem.CodeInvalidArgument {
			t.Errorf("%s = %d %+v", url, code, e)
		}
	}
}

func TestAllowList(t *testing.T) {
	allow, err := allowList("127.0.0.1, 10.1.2.3")
	if err != nil || !allow(netip.MustParseAddr("10.1.2.3")) || allow(netip.MustParseAddr("10.1.2.4")) {
		t.Errorf("allowList = %v", err)
	}
	if allow, err := allowList(""); allow != nil || err != nil {
		t.Error("empty list allowed something")
	}
	if _, err := allowList("not-an-ip"); err == nil {
		t.Error("bad address accepted")
	}
}
