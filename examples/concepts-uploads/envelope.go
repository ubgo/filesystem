package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/recordshttp"
	"github.com/ubgo/filesystem/records/share"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of the envelope step: the records owner the upload route
// attaches to, and the scope and link lifetime of its Store.
const (
	envelopeOwner = "user"
	envelopeScope = "acme"
	envelopeLink  = time.Hour
)

// dataEnvelope writes every success as {"data": …}, the envelope the rest
// of the API uses. One generic function serves the three handler
// packages: each has its own closed SuccessBody, and
// dataEnvelope[upload.SuccessBody], dataEnvelope[recordshttp.SuccessBody]
// and dataEnvelope[share.SuccessBody] are each package's SuccessWriter.
// ds:def id=dataenvelope-q7w3n8kd
func dataEnvelope[B any](w http.ResponseWriter, _ *http.Request, status int, body B) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Data B `json:"data"`
	}{body})
}

// envelopeHandlers wires the upload form handler, the records endpoints
// and the share handler with the same envelope (respond true) or with
// each package's default JSON (false).
// ds:def id=envelopehandlers-h2m8v4xc
func envelopeHandlers(disk *filesystem.Disk, s *records.Store, sp *share.Plugin, respond bool) (uploads, media, links http.Handler, err error) {
	uo := upload.HandlerOptions{}
	rc := recordshttp.Config{
		Store: s, Disk: disk.Name(), Policy: policy,
		Scope:     func(*http.Request) (string, error) { return envelopeScope, nil },
		Authorize: func(*http.Request, recordshttp.Action, recordshttp.Target) error { return nil }, // the app's own rule here
	}
	var so []share.HandlerOption
	if respond {
		uo.Respond = dataEnvelope[upload.SuccessBody]
		rc.Respond = dataEnvelope[recordshttp.SuccessBody]
		so = append(so, share.WithRespond(dataEnvelope[share.SuccessBody]))
	}
	media, err = recordshttp.Handler(rc)
	if err != nil {
		return nil, nil, nil, err
	}
	return upload.Handler(disk, policy, uo), media, share.Handler(s, sp, so...), nil
}

// newEnvelopeStore is a Store on an in-memory SQLite database with the
// folders and share plugins, so a folder can be shared and listed.
func newEnvelopeStore(ctx context.Context, db *sql.DB) (*filesystem.Disk, *records.Store, *folders.Plugin, *share.Plugin, error) {
	signer, err := signedurl.New("https://app.example.com/files", []signedurl.Key{{ID: "k1", Secret: demoSecret}})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	disk, err := filesystem.NewDisk("uploads", memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, nil, nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	fp := folders.New()
	sp := share.New(share.WithCollections(fp.Collection(), share.Owners{}))
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(envelopeOwner), records.WithPlugins(fp, sp))
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, nil, nil, err
	}
	return disk, s, fp, sp, nil
}

// shape describes a JSON body by its keys, so the printout shows the
// envelope without the random keys inside: "files" for {"files": …},
// "data.files" for {"data": {"files": …}}, "[1]" for an array of one and
// "data[1]" for {"data": [one]}.
func shape(body []byte) string {
	var arr []json.RawMessage
	if json.Unmarshal(body, &arr) == nil {
		return fmt.Sprintf("[%d]", len(arr))
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return "not JSON"
	}
	keys := slices.Sorted(maps.Keys(obj))
	if len(keys) != 1 {
		return strings.Join(keys, ",")
	}
	inner := shape(obj[keys[0]])
	if strings.HasPrefix(inner, "[") {
		return keys[0] + inner
	}
	return keys[0] + "." + inner
}

// sendForm posts one file in the multipart field "file" (both handlers'
// field) to path on h.
func sendForm(ctx context.Context, h http.Handler, path string, f formFile) (int, []byte) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile(upload.DefaultField, f.name)
	_, _ = io.WriteString(part, f.data)
	_ = mw.Close()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// envelope answers the same requests through each handler without and
// with Respond: an upload form, a records upload into a user's role, and
// a shared folder's listing.
// ds:def id=envelope-k5t9w2rb
func envelope(ctx context.Context, out io.Writer) error {
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	disk, s, fp, sp, err := newEnvelopeStore(ctx, db)
	if err != nil {
		return err
	}
	folder, err := fp.Create(ctx, s, envelopeScope, folders.RootID, "Press kit")
	if err != nil {
		return err
	}
	f, err := s.Upload(ctx, envelopeScope, disk.Name(), strings.NewReader(pdfFile), "kit.pdf", policy)
	if err != nil {
		return err
	}
	if err := fp.Put(ctx, s, envelopeScope, f.ID, folder.ID); err != nil {
		return err
	}
	link, err := sp.CreateFor(ctx, s, envelopeScope, share.Target{Kind: folders.KindFolder, Ref: folder.ID}, share.Options{TTL: envelopeLink})
	if err != nil {
		return err
	}
	for _, respond := range []bool{false, true} {
		uploads, media, links, err := envelopeHandlers(disk, s, sp, respond)
		if err != nil {
			return err
		}
		upStatus, upBody := sendForm(ctx, uploads, "/upload", formFile{"a.png", pngFile})
		mediaStatus, mediaBody := sendForm(ctx, media, "/owners/"+envelopeOwner+"/u1/docs", formFile{"b.pdf", pdfFile})
		rec := httptest.NewRecorder()
		links.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/"+link.Token, nil))
		fmt.Fprintf(out, "envelope respond=%t: upload %d %s, records %d %s, share listing %d %s\n",
			respond, upStatus, shape(upBody), mediaStatus, shape(mediaBody), rec.Code, shape(rec.Body.Bytes()))
	}
	return nil
}
