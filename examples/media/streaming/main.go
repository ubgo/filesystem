// Command streaming hands an uploaded video to a hosted video platform
// with contrib/videoplatform: Publish asks the platform to fetch the file
// through a short-lived signed URL and records the asset as preparing, the
// platform's signed webhook marks it ready, and the app reads back the
// HLS and thumbnail URLs a player needs. The platform here is Mux, through
// contrib/videoplatform/mux, pointed at a fake Mux API served in this
// process, so the program needs no account. Cloudflare Stream (cfstream)
// plugs in the same way.
//
//	go run ./streaming      (from examples/media)
package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/sqlb"
	_ "modernc.org/sqlite" // the records tables live in in-memory SQLite

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/videoplatform"
	"github.com/ubgo/filesystem/contrib/videoplatform/mux"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
)

// Decisions of this example, not of the library.
const (
	diskMedia = "media"
	scope     = "demo"
	// signBase is where the app serves signed downloads; the platform
	// fetches the video from a URL under it.
	signBase = "https://app.example.com/files"
)

// Credentials of the fake platform. A real app loads them from its secret
// store; videoplatform.Secret keeps them out of logs and %v.
var (
	tokenSecret   = videoplatform.NewSecret("mux-token-secret")
	webhookSecret = videoplatform.NewSecret("mux-webhook-secret")
	signingKey    = []byte("0123456789abcdef0123456789abcdef")
)

// newPlatform is the Mux adapter. BaseURL points it at the fake API; a
// real app leaves it empty for https://api.mux.com.
// ds:def id=newplatform-m6q2x8vd
func newPlatform(baseURL string) (*mux.Client, error) {
	return mux.New(mux.Config{
		TokenID:       "mux-token-id",
		TokenSecret:   tokenSecret,
		WebhookSecret: webhookSecret,
		Policy:        mux.PolicyPublic,
		BaseURL:       baseURL,
	})
}

// newStore opens a Store with the videoplatform plugin installed, over a
// disk that signs URLs: the platform downloads the video from one.
// ds:def id=newstore-z3k9w5tf
func newStore(ctx context.Context, platform videoplatform.Platform) (*records.Store, *videoplatform.Plugin, error) {
	signer, err := signedurl.New(signBase, []signedurl.Key{{ID: "k1", Secret: signingKey}})
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskMedia, memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1) // one connection keeps the in-memory database alive
	plugin := videoplatform.New(platform, videoplatform.WithSourceTTL(time.Hour))
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(plugin))
	if err != nil {
		return nil, nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return store, plugin, nil
}

// videos accepts MP4 uploads. The bytes are not decoded here: the
// platform transcodes them.
var videos = upload.Policy{MaxSize: 500 << 20, Allowed: []string{"video/mp4"}, Key: upload.RandomKey("videos")}

// publish uploads a video and hands it to the platform. The file's
// records status is processing until the platform reports back.
// ds:def id=publish-c4n8r2hy
func publish(ctx context.Context, s *records.Store, p *videoplatform.Plugin, body io.Reader) (records.File, videoplatform.Published, error) {
	f, err := s.Upload(ctx, scope, diskMedia, body, "clip.mp4", videos)
	if err != nil {
		return records.File{}, videoplatform.Published{}, err
	}
	v, err := p.Publish(ctx, s, scope, f.ID)
	return f, v, err
}

// mountWebhook is the route the platform calls with progress. The handler
// verifies the signature, records the asset's state, and sets the file's
// status to ready or failed.
// ds:def id=mountwebhook-t7v3m9kq
func mountWebhook(mx *http.ServeMux, s *records.Store, p *videoplatform.Plugin) {
	mx.Handle("POST /hooks/mux", p.Webhook(s, nil))
}

// mp4 is the start of an MP4 file: enough for the type to be detected
// from the bytes. The fake platform never decodes it.
var mp4 = append([]byte{0, 0, 0, 0x18}, []byte("ftypmp42\x00\x00\x00\x00mp42isom")...)

// run uploads, publishes, delivers the ready webhook and prints the
// asset at each step.
func run(ctx context.Context, w io.Writer) error {
	api := newFakeMux()
	srv := httptest.NewServer(api)
	defer srv.Close()
	platform, err := newPlatform(srv.URL)
	if err != nil {
		return err
	}
	s, p, err := newStore(ctx, platform)
	if err != nil {
		return err
	}
	f, v, err := publish(ctx, s, p, bytes.NewReader(mp4))
	if err != nil {
		return err
	}
	signed := strings.HasPrefix(api.fetched, signBase+"/") && strings.Contains(api.fetched, "sig=")
	fmt.Fprintf(w, "published %s on %s: %s, fetched from a signed URL %t\n", v.Asset.ID, v.Platform, v.Asset.Status, signed)
	f, err = s.File(ctx, scope, f.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "file status: %s\n", f.Status)
	app := http.NewServeMux()
	mountWebhook(app, s, p)
	code := deliver(app, api.readyEvent(v.Asset.ID), webhookSecret.Reveal())
	fmt.Fprintf(w, "webhook: %d\n", code)
	code = deliver(app, api.readyEvent(v.Asset.ID), []byte("wrong secret"))
	fmt.Fprintf(w, "forged webhook: %d\n", code)
	v, err = p.Asset(ctx, s, scope, f.ID)
	if err != nil {
		return err
	}
	f, err = s.File(ctx, scope, f.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "asset %s, file %s, %s\nhls %s\nthumbnail %s\n", v.Asset.Status, f.Status, v.Asset.Duration, v.Asset.HLS, v.Asset.Thumbnail)
	return nil
}

// deliver posts a webhook body to the app, signed with secret as Mux
// signs: "t=<unix seconds>,v1=<hex HMAC-SHA256 of t.body>".
func deliver(app http.Handler, body, secret []byte) int {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, "/hooks/mux", bytes.NewReader(body))
	req.Header.Set("Mux-Signature", fmt.Sprintf("t=%s,v1=%x", ts, videoplatform.Sign(secret, ts, body)))
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	return rec.Code
}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatal(err)
	}
}
