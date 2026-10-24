// Package tus serves resumable uploads over the tus protocol
// (https://tus.io), the one mobile apps and uploaders such as Uppy use to
// survive dropped connections, by embedding tusd rather than reimplementing
// the protocol.
//
// tusd keeps each upload session in a local temporary directory. When the
// last byte arrives, and before the client is told it succeeded, the file
// goes through the same upload.Policy as every other upload path (type
// detected from the bytes, size cap) into the target disk, and OnStored
// runs (to record or attach it). If any of that fails, the client's final
// request fails with a matching status, nothing stays on the disk, and the
// session is deleted. The stored key is returned in the Filesystem-Key
// header.
//
// Config.Authorize, when set, runs on every request but CORS preflights:
// the POST that creates an upload and the PATCH, HEAD and DELETE requests
// that continue, resume or cancel it. Config.Resolve chooses the policy of
// each upload, at creation and at the final PATCH, which is how an upload
// session (upload.Sessions) applies its key prefix and limits to tus.
package tus

import (
	"cmp"
	"context"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/tus/tusd/v2/pkg/filelocker"
	"github.com/tus/tusd/v2/pkg/filestore"
	"github.com/tus/tusd/v2/pkg/handler"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/upload"
)

// HeaderKey carries the stored key in the response to the final PATCH.
const HeaderKey = "Filesystem-Key"

// Metadata keys read from the client's Upload-Metadata. Uppy and the
// official tus clients send these names.
const (
	MetaFilename = "filename"
	MetaFiletype = "filetype"
)

// tus error codes in responses, for clients that branch on them.
const (
	errCodeDeferred  = "ERR_LENGTH_REQUIRED"
	errCodeForbidden = "ERR_FORBIDDEN"
	// errCodeMisconfigured answers a Resolve that returned an invalid
	// policy, as code unknown does in the upload handlers.
	errCodeMisconfigured = "ERR_UNKNOWN"
)

// HeaderUploadSession is the request header an upload session's token
// travels in (upload.Sessions); the default CORS policy allows it, so a
// browser on another origin can send it.
const HeaderUploadSession = "Upload-Session"

// SessionStore keeps unfinished upload sessions: a tusd data store that
// can also terminate an upload (the bridge deletes every session it has
// finished with) and installs itself in a composer. tusd's filestore,
// s3store, gcsstore and azurestore all are one.
type SessionStore interface {
	handler.DataStore
	handler.TerminaterDataStore
	UseIn(composer *handler.StoreComposer)
}

// DefaultCors returns tusd's default CORS policy (every origin, no
// credentials) with HeaderUploadSession added to the allowed headers, the
// policy New applies when Config.Cors is nil. Start from it to restrict
// origins or allow credentials and keep the headers tus needs.
func DefaultCors() *handler.CorsConfig {
	c := handler.DefaultCorsConfig
	c.AllowHeaders += ", " + HeaderUploadSession
	return &c
}

// Config configures the handler.
type Config struct {
	// Disk receives finished uploads. Required.
	Disk *filesystem.Disk
	// Policy is checked when an upload is created (declared size and type,
	// the client's word) and enforced when it finishes (the bytes).
	// Required.
	Policy upload.Policy
	// TempDir holds unfinished upload sessions on local disk, with
	// tusd's filestore, and their locks, with tusd's filelocker, which
	// also keeps two PATCHes from writing one upload at once. Required
	// unless Store is set. Sessions a client abandons stay here until
	// Sweep deletes them; run it on a schedule. Use the directory for
	// nothing else.
	TempDir string
	// Store, when set, keeps unfinished upload sessions instead of a
	// filestore in TempDir: tusd's s3store, gcsstore or azurestore, so
	// several instances behind a load balancer without sticky sessions
	// can each continue an upload. It needs Locker. Sweep does not
	// reach it: expire its sessions with the backend's own lifecycle
	// rules.
	Store SessionStore
	// Locker, when set, locks each upload while a request works on it,
	// instead of tusd's filelocker in TempDir: one all instances share
	// (Redis, etcd) when Store is shared. Required with Store; with a
	// TempDir store it replaces the filelocker.
	Locker handler.Locker
	// Cors is the handler's CORS policy; nil means DefaultCors(): every
	// origin, no credentials, and the tus headers plus
	// HeaderUploadSession allowed. Set AllowOrigin to restrict origins;
	// AllowCredentials sends cookies cross-origin; Disable leaves CORS to
	// a proxy in front. A custom AllowHeaders must keep tusd's own
	// headers and add HeaderUploadSession when Resolve reads it.
	Cors *handler.CorsConfig
	// BasePath is the path the handler is mounted at, e.g. "/uploads/"; tus
	// builds upload URLs from it. Required.
	BasePath string
	// Authorize, when set, runs on every request (creation, PATCH, HEAD
	// and DELETE; not CORS preflights, which carry no credentials) with
	// its context and headers; an error refuses the request with 403. It
	// checks who is calling, not that they created the upload: the upload
	// URL, unguessable, remains what ties a session to its creator.
	Authorize func(ctx context.Context, header http.Header) error
	// AuthorizeUpload, when set, runs on every request but CORS
	// preflights, after Authorize, with the upload the request is about:
	// its ID and its stored metadata (on creation, ID "" and the metadata
	// the client declares). An error refuses the request with 403. It is
	// the per-upload check Authorize cannot make: that the caller owns
	// the upload it continues, reads or deletes. Pair it with
	// StampMetadata, so the owner it compares against was written by the
	// server, not claimed by the client. An upload whose metadata cannot
	// be read (an unknown ID, a store failure) is refused with 403
	// without calling it.
	AuthorizeUpload func(ctx context.Context, req UploadRequest) error
	// StampMetadata, when set, runs when an upload is created and returns
	// metadata the server sets on it, over any key of the same name the
	// client sent: the owner's user or tenant ID, for AuthorizeUpload to
	// check later. An error refuses the creation with 403. Stamp the same
	// keys on every creation: a key it leaves out keeps whatever the
	// client sent.
	StampMetadata func(ctx context.Context, header http.Header) (map[string]string, error)
	// Resolve, when set, chooses the policy of each upload from Policy,
	// as the upload handlers' Resolve does: upload.Sessions.Resolver()
	// applies an upload session's key prefix and limits (the client sends
	// its Upload-Session header with every request). It runs twice per
	// upload: at creation, against the declared size and type, and at the
	// final PATCH, whose policy the bytes are stored under, so a session
	// that expired meanwhile stops the upload there. It must give one
	// upload the same policy both times, and may only narrow Policy:
	// tusd caps every upload at Policy.MaxSize. Its error refuses the
	// request with its code's status; a resolved policy that fails
	// Validate is the server's mistake, answered 500. nil uses Policy.
	Resolve upload.Resolver
	// OnStored, when set, runs after the file is stored, typically to
	// record and attach it. An error deletes the stored object and fails
	// the client's final request with the status of its code
	// (fshttp.StatusOf: a quota_exceeded is 507, an unclassified error
	// 500).
	OnStored func(ctx context.Context, res upload.Result, meta map[string]string) error
	// Logger receives tusd's own logs: a line per request and response
	// at Info, internal failures at Error. Nil means slog.Default(), as in
	// tusd; pass a logger whose handler drops Info to keep only failures.
	Logger *slog.Logger
	// RespectForwardedHeaders builds the upload URLs tusd returns
	// (Location) from X-Forwarded-Host, X-Forwarded-Proto and Forwarded,
	// for a handler behind a TLS-terminating proxy, which otherwise yields
	// http:// URLs with the internal host. Enable it only behind a proxy
	// that sets or strips those headers: a client could otherwise choose
	// the host its upload URL points at.
	RespectForwardedHeaders bool
}

// UploadRequest is what AuthorizeUpload is asked about.
type UploadRequest struct {
	// Method is the request's method: POST to create, PATCH to append,
	// HEAD to read the offset, DELETE to terminate.
	Method string
	// Header is the request's headers.
	Header http.Header
	// UploadID is the upload the request names; "" when it creates one.
	UploadID string
	// Metadata is the upload's stored metadata (StampMetadata's keys
	// included), or on creation the client's Upload-Metadata. Never nil.
	Metadata map[string]string
}

// New returns the tus handler. Mount it under BasePath with the prefix
// stripped, as tusd expects:
//
//	h, _ := tus.New(cfg) // cfg.BasePath = "/uploads/"
//	mux.Handle("/uploads/", http.StripPrefix("/uploads/", h))
func New(cfg Config) (http.Handler, error) {
	if cfg.Disk == nil || cfg.BasePath == "" || (cfg.TempDir == "" && cfg.Store == nil) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "tus: Disk, BasePath and TempDir (or Store) are required")
	}
	if cfg.Store != nil && cfg.Locker == nil {
		// A shared store with a lock local to one instance would leave
		// concurrent PATCHes on two instances unguarded.
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "tus: Store needs a Locker")
	}
	if err := cfg.Policy.Validate(); err != nil {
		return nil, err
	}
	composer, store, err := compose(cfg)
	if err != nil {
		return nil, err
	}
	b := &bridge{cfg: cfg, store: store}
	h, err := handler.NewHandler(handler.Config{
		BasePath:                  cfg.BasePath,
		StoreComposer:             composer,
		Cors:                      cmp.Or(cfg.Cors, DefaultCors()),
		MaxSize:                   cfg.Policy.MaxSize,
		DisableDownload:           true,
		Logger:                    tusdLogger(cmp.Or(cfg.Logger, slog.Default())),
		RespectForwardedHeaders:   cfg.RespectForwardedHeaders,
		PreUploadCreateCallback:   b.preCreate,
		PreFinishResponseCallback: b.preFinish,
	})
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	if cfg.Authorize == nil && cfg.AuthorizeUpload == nil {
		return h, nil
	}
	return authorize(h, b.check), nil
}

// check runs Authorize, then AuthorizeUpload with the upload r names.
func (b *bridge) check(r *http.Request) error {
	if b.cfg.Authorize != nil {
		if err := b.cfg.Authorize(r.Context(), r.Header); err != nil {
			return err
		}
	}
	if b.cfg.AuthorizeUpload == nil {
		return nil
	}
	// Metadata is a copy, never nil: the callback may read it freely and
	// cannot change the upload's stored metadata.
	req := UploadRequest{Method: r.Method, Header: r.Header, UploadID: uploadID(r.URL.Path), Metadata: map[string]string{}}
	if req.UploadID == "" {
		maps.Copy(req.Metadata, handler.ParseMetadataHeader(r.Header.Get("Upload-Metadata")))
	} else {
		u, err := b.store.GetUpload(r.Context(), req.UploadID)
		if err != nil {
			return err
		}
		info, err := u.GetInfo(r.Context())
		if err != nil {
			return err
		}
		maps.Copy(req.Metadata, info.MetaData)
	}
	return b.cfg.AuthorizeUpload(r.Context(), req)
}

// uploadID is the upload a request path names: its last segment, the
// handler being mounted with its base path stripped. "" is a creation.
func uploadID(p string) string {
	p = strings.Trim(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}
	return p
}

// compose builds the store composer: cfg.Store, or a filestore in
// TempDir, and cfg.Locker, or a filelocker in TempDir.
func compose(cfg Config) (*handler.StoreComposer, SessionStore, error) {
	var store SessionStore = cfg.Store
	if store == nil {
		if err := os.MkdirAll(cfg.TempDir, 0o700); err != nil {
			return nil, nil, err
		}
		store = filestore.New(cfg.TempDir)
	}
	composer := handler.NewStoreComposer()
	store.UseIn(composer)
	if cfg.Locker != nil {
		composer.UseLocker(cfg.Locker)
	} else {
		filelocker.New(cfg.TempDir).UseIn(composer)
	}
	return composer, store, nil
}

// errForbidden is the answer to a request Authorize refused.
var errForbidden = handler.NewError(errCodeForbidden, "upload not authorised", http.StatusForbidden)

// authorize runs check on every request but CORS preflights. tusd has
// hooks only for creation, completion and termination, so the check
// wraps the handler instead; a refusal is still written inside tusd's
// middleware, so it carries the CORS and Tus-Resumable headers a browser
// client needs to read the 403 rather than see a network error.
func authorize(h *handler.Handler, check func(r *http.Request) error) http.Handler {
	refuse := h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range errForbidden.HTTPResponse.Header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(errForbidden.HTTPResponse.StatusCode)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(errForbidden.HTTPResponse.Body))
		}
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodOptions {
			if err := check(r); err != nil {
				refuse.ServeHTTP(w, r)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

type bridge struct {
	cfg   Config
	store SessionStore
}

// reject builds the tus error a hook returns to refuse a request. The
// message is fshttp.ClientMessage's: the cause of a code that describes the
// client's own upload, the status text otherwise, never the operation, disk
// or storage key that err.Error() would carry.
func reject(err error, status int) handler.Error {
	code := filesystem.CodeOf(err)
	return handler.NewError("ERR_"+strings.ToUpper(string(code)), fshttp.ClientMessage(err), status)
}

// preCreate refuses uploads the policy cannot accept from what the client
// declares: no length, a length over the resolved policy's MaxSize, or a
// refused type. Authorisation ran before tusd (see authorize).
func (b *bridge) preCreate(hook handler.HookEvent) (handler.HTTPResponse, handler.FileInfoChanges, error) {
	none := handler.HTTPResponse{}
	if hook.Upload.SizeIsDeferred {
		return none, handler.FileInfoChanges{}, handler.NewError(errCodeDeferred, "Upload-Length is required", http.StatusBadRequest)
	}
	p, err := b.policy(hook)
	if err != nil {
		return none, handler.FileInfoChanges{}, err
	}
	// A declared length over the base policy's MaxSize never reaches this
	// hook: tusd itself refuses it with 413, because MaxSize is passed to
	// it. A resolved policy may be narrower.
	if hook.Upload.Size > p.MaxSize {
		err := filesystem.Errorf(filesystem.CodeTooLarge, "the upload is larger than %d bytes", p.MaxSize)
		return none, handler.FileInfoChanges{}, reject(err, http.StatusRequestEntityTooLarge)
	}
	if t := hook.Upload.MetaData[MetaFiletype]; t != "" {
		if err := p.Accepts(t); err != nil {
			return none, handler.FileInfoChanges{}, reject(err, http.StatusUnsupportedMediaType)
		}
	}
	if b.cfg.StampMetadata == nil {
		return none, handler.FileInfoChanges{}, nil
	}
	stamp, err := b.cfg.StampMetadata(hook.Context, hook.HTTPRequest.Header)
	if err != nil {
		return none, handler.FileInfoChanges{}, errForbidden
	}
	meta := handler.MetaData{}
	maps.Copy(meta, hook.Upload.MetaData)
	maps.Copy(meta, stamp)
	return none, handler.FileInfoChanges{MetaData: meta}, nil
}

// policy is the policy of the request hook reports: Config.Policy, or
// what Config.Resolve makes of it, validated. Its error is the tus error
// to answer with.
func (b *bridge) policy(hook handler.HookEvent) (upload.Policy, error) {
	if b.cfg.Resolve == nil {
		return b.cfg.Policy, nil
	}
	p, err := b.cfg.Resolve(requestOf(hook), b.cfg.Policy)
	if err != nil {
		return upload.Policy{}, reject(err, fshttp.StatusOf(err))
	}
	// A resolved policy that fails validation is the server's mistake:
	// 500 with the status text only, as the upload handlers answer it.
	if err := p.Validate(); err != nil {
		return upload.Policy{}, handler.NewError(errCodeMisconfigured, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
	return p, nil
}

// requestOf rebuilds, for a Resolver, the request a tusd hook reports:
// its method, URI, remote address, headers and context. It has no body.
func requestOf(hook handler.HookEvent) *http.Request {
	r := &http.Request{
		Method: hook.HTTPRequest.Method, RequestURI: hook.HTTPRequest.URI, RemoteAddr: hook.HTTPRequest.RemoteAddr,
		Header: hook.HTTPRequest.Header, URL: &url.URL{},
	}
	// The URI is one net/http already parsed for tusd, so it parses.
	if u, err := url.ParseRequestURI(hook.HTTPRequest.URI); err == nil {
		r.URL = u
	}
	return r.WithContext(hook.Context)
}

// preFinish promotes a finished upload into the disk before the client
// hears it succeeded. The session is deleted whatever the outcome: a
// refused file must not linger, and a stored one is no longer needed.
func (b *bridge) preFinish(hook handler.HookEvent) (handler.HTTPResponse, error) {
	ctx := hook.Context
	p, err := b.policy(hook)
	if err != nil {
		b.terminate(ctx, hook.Upload.ID)
		return handler.HTTPResponse{}, err
	}
	res, err := b.promote(ctx, hook.Upload, p)
	b.terminate(ctx, hook.Upload.ID)
	if err != nil {
		return handler.HTTPResponse{}, reject(err, fshttp.StatusOf(err))
	}
	return handler.HTTPResponse{Header: handler.HTTPHeader{HeaderKey: res.Key}}, nil
}

// promote stores the finished upload info under policy p and runs
// OnStored.
func (b *bridge) promote(ctx context.Context, info handler.FileInfo, p upload.Policy) (upload.Result, error) {
	up, err := b.store.GetUpload(ctx, info.ID)
	if err != nil {
		return upload.Result{}, err
	}
	rc, err := up.GetReader(ctx)
	if err != nil {
		return upload.Result{}, err
	}
	defer func() { _ = rc.Close() }()
	res, err := upload.Receive(ctx, b.cfg.Disk, rc, info.MetaData[MetaFilename], p)
	if err != nil {
		return upload.Result{}, err
	}
	if b.cfg.OnStored != nil {
		if err := b.cfg.OnStored(ctx, res, info.MetaData); err != nil {
			_ = b.cfg.Disk.Delete(context.WithoutCancel(ctx), res.Key)
			return upload.Result{}, err // its code picks the status, as in upload.Handler
		}
	}
	return res, nil
}

func (b *bridge) terminate(ctx context.Context, id string) {
	if up, err := b.store.GetUpload(ctx, id); err == nil {
		_ = b.store.AsTerminatableUpload(up).Terminate(ctx)
	}
}
