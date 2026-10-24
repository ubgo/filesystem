package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
)

func setup(t *testing.T, data Source) (*App, string) {
	t.Helper()
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	app, err := New(t.TempDir(), srv.URL, bytes.Repeat([]byte("k"), 32), data)
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = app.Handler
	return app, srv.URL
}

func post(t *testing.T, base, user string) (int, exportResponse) {
	t.Helper()
	resp, err := http.Post(base+"/users/"+user+"/exports", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out exportResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestExportDownloadAndReuse(t *testing.T) {
	_, base := setup(t, demoData{})
	code, first := post(t, base, "u1")
	if code != http.StatusCreated || !first.Created {
		t.Fatalf("first = %d %+v", code, first)
	}
	resp, err := http.Get(first.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, "export-") {
		t.Errorf("disposition = %q", cd)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil || len(zr.File) != 2 || zr.File[1].Name != "orders.csv" {
		t.Fatalf("zip = %v, %v", zr, err)
	}
	code, again := post(t, base, "u1")
	if code != http.StatusOK || again.Created || again.Key != first.Key {
		t.Errorf("same day = %d %+v", code, again)
	}
	if code, _ := post(t, base, "../u1"); code != http.StatusNotFound && code != http.StatusBadRequest {
		t.Errorf("traversal id = %d", code)
	}
	if code, _ := post(t, base, "U%201"); code != http.StatusBadRequest {
		t.Errorf("invalid id = %d", code)
	}
}

func TestConcurrentRequestsMakeOneExport(t *testing.T) {
	app, _ := setup(t, demoData{})
	var wg sync.WaitGroup
	created := make([]bool, 6)
	for i := range created {
		wg.Go(func() {
			_, c, err := app.Export(t.Context(), "u1")
			if err != nil {
				t.Error(err)
			}
			created[i] = c
		})
	}
	wg.Wait()
	n := 0
	for _, c := range created {
		if c {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d requests created the export, want 1", n)
	}
}

// failingData fails half-way, after the profile was read.
type failingData struct{ demoData }

func (failingData) Orders(context.Context, string) ([]Order, error) {
	return nil, errors.New("database down")
}

func TestFailedExportStoresNothing(t *testing.T) {
	app, _ := setup(t, failingData{})
	if _, _, err := app.Export(t.Context(), "u1"); err == nil {
		t.Fatal("export succeeded")
	}
	if ok, _ := app.Disk.Exists(t.Context(), exportKey("u1", app.Now())); ok {
		t.Error("a failed export left an object")
	}
}

func TestNextDayAndSweep(t *testing.T) {
	app, _ := setup(t, demoData{})
	day := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	app.Now = func() time.Time { return day }
	k1, _, _ := app.Export(t.Context(), "u1")
	app.Now = func() time.Time { return day.Add(24 * time.Hour) }
	k2, created, _ := app.Export(t.Context(), "u1")
	if !created || k1 == k2 {
		t.Errorf("next day reused %s", k1)
	}
	if n, err := app.Sweep(t.Context()); err != nil || n != 0 {
		t.Errorf("sweep of fresh exports = %d, %v", n, err)
	}
	app.Now = func() time.Time { return time.Now().Add(retention + time.Hour) }
	if n, err := app.Sweep(t.Context()); err != nil || n != 2 {
		t.Errorf("sweep after retention = %d, %v", n, err)
	}
	if _, err := app.Disk.Stat(t.Context(), k2); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("swept export still there: %v", err)
	}
}
