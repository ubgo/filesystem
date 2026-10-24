package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tus/tusd/v2/pkg/filestore"
	"github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/memorylocker"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/tus"
	"github.com/ubgo/filesystem/records"
)

// sharedCommand is the argument that runs the two-instance demo instead
// of the server.
const sharedCommand = "shared"

// newSharedTus configures one instance's tus handler over a session
// store and a locker every instance shares, so a chunk may land on any
// instance behind a load balancer. In production the store is tusd's
// s3store (or gcsstore, azurestore) and the locker a Redis one; here a
// filestore on a directory both instances use and one in-process
// memorylocker stand in for them.
// ds:def id=app-newsharedtus-r8k3w6vn
func (a *App) newSharedTus(disk *filesystem.Disk, store tus.SessionStore, locker handler.Locker) (http.Handler, error) {
	return tus.New(tus.Config{
		Disk:     disk,
		Policy:   policy,
		Store:    store,
		Locker:   locker, // required with Store: a lock local to one instance stops nothing
		BasePath: tusPath,
		Authorize: func(ctx context.Context, _ http.Header) error {
			_, err := userFrom(ctx)
			return err
		},
		OnStored: a.record,
		Logger:   quietLog, // tusd logs every request at Info
	})
}

// quietLog keeps tusd's warnings and errors only.
var quietLog = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

// sharedInstances runs two instances over one shared session store: the
// upload is created and half sent on instance a, then resumed and
// finished on instance b. It also shows New refusing a shared store
// without a locker.
// ds:def id=sharedinstances-p5t9c2xh
func sharedInstances(ctx context.Context, out io.Writer, dataDir string) error {
	st, disk, err := openStore(ctx, dataDir)
	if err != nil {
		return err
	}
	app := &App{Store: st}
	sessions := filestore.New(filepath.Join(dataDir, "shared-sessions"))
	locker := memorylocker.New()
	bases := map[string]string{}
	for _, name := range []string{"a", "b"} {
		h, err := app.newSharedTus(disk, sessions, locker)
		if err != nil {
			return err
		}
		mux := http.NewServeMux()
		mux.Handle(tusPath, http.StripPrefix(tusPath, h))
		srv := httptest.NewServer(withUser(mux))
		defer srv.Close()
		bases[name] = srv.URL
	}
	body := jpegSample
	half := len(body) / 2
	created, err := tusRequest(ctx, http.MethodPost, bases["a"]+tusPath, "", map[string]string{
		"Upload-Length": strconv.Itoa(len(body)), "Upload-Metadata": "filename " + base64.StdEncoding.EncodeToString([]byte("clip.jpg")),
	})
	if err != nil {
		return err
	}
	loc, err := url.Parse(created.Header.Get("Location"))
	if err != nil {
		return err
	}
	first, err := tusPatch(ctx, bases["a"]+loc.Path, 0, body[:half])
	if err != nil {
		return err
	}
	head, err := tusRequest(ctx, http.MethodHead, bases["b"]+loc.Path, "", nil)
	if err != nil {
		return err
	}
	last, err := tusPatch(ctx, bases["b"]+loc.Path, half, body[half:])
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "instance a: create %d, first %d bytes %d\n", created.StatusCode, half, first.StatusCode)
	fmt.Fprintf(out, "instance b: HEAD sees offset %s, last %d bytes %d\n", head.Header.Get("Upload-Offset"), len(body)-half, last.StatusCode)
	attached, err := st.Attachments(ctx, "", records.Owner{Type: ownerUser, ID: demoUser}, roleUploads)
	if err != nil {
		return err
	}
	if len(attached) != 1 {
		return fmt.Errorf("recorded %d files, want 1", len(attached))
	}
	f := attached[0].File
	fmt.Fprintf(out, "recorded for %s: %s, %d bytes\n", demoUser, f.ContentType, f.Size)
	_, err = tus.New(tus.Config{Disk: disk, Policy: policy, Store: sessions, BasePath: tusPath})
	fmt.Fprintf(out, "shared store without a locker: %s\n", filesystem.CodeOf(err))
	return nil
}

// demoUser is who the two-instance demo uploads as.
const demoUser = "u1"

// jpegSample sniffs as a JPEG; any type the policy allows would do.
const jpegSample = "\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01-one-upload-across-two-instances"

// tusPatch sends one chunk at offset.
func tusPatch(ctx context.Context, rawURL string, offset int, chunk string) (*http.Response, error) {
	return tusRequest(ctx, http.MethodPatch, rawURL, chunk, map[string]string{
		"Upload-Offset": strconv.Itoa(offset), "Content-Type": "application/offset+octet-stream",
	})
}

// tusRequest sends one tus request as demoUser.
func tusRequest(ctx context.Context, method, rawURL, body string, h map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set(headerUser, demoUser)
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp, resp.Body.Close()
}
