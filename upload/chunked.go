package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/sniff"
)

// ChunkFields names the multipart form fields of one chunk, which differ
// between client libraries. Every field must come before the file part,
// as Dropzone and resumable.js send them.
type ChunkFields struct {
	// UploadID is the client's identifier of the whole file.
	UploadID string
	// Index is the chunk's number, counted from IndexBase.
	Index string
	// Total is how many chunks the file has.
	Total string
	// TotalSize is the whole file's size in bytes.
	TotalSize string
	// File is the field holding the chunk's bytes.
	File string
	// IndexBase is the first chunk's number: 0 for Dropzone, 1 for
	// resumable.js.
	IndexBase int
}

// DropzoneFields are the field names of Dropzone's chunked uploads (and
// FilePond configured the same way).
func DropzoneFields() ChunkFields {
	return ChunkFields{UploadID: "dzuuid", Index: "dzchunkindex", Total: "dztotalchunkcount", TotalSize: "dztotalfilesize", File: DefaultField}
}

// ResumableFields are the field names of resumable.js and flow.js.
func ResumableFields() ChunkFields {
	return ChunkFields{UploadID: "resumableIdentifier", Index: "resumableChunkNumber", Total: "resumableTotalChunks", TotalSize: "resumableTotalSize", File: DefaultField, IndexBase: 1}
}

// Limits of chunked uploads.
const (
	// DefaultMaxChunkBytes caps one chunk unless ChunkedOptions says
	// otherwise; client libraries default to 1 to 10 MB.
	DefaultMaxChunkBytes = 16 << 20
	// MaxChunks bounds the chunks of one file.
	MaxChunks = 10000
	// DefaultMaxUploadsPerOwner is how many chunked uploads one owner may
	// have in progress unless ChunkedOptions says otherwise.
	DefaultMaxUploadsPerOwner = 16
	// DefaultAssemblyTimeout is how long an assembly may run before
	// another request may take it over.
	DefaultAssemblyTimeout = 15 * time.Minute
	// manifestAttempts bounds the compare-and-swap retries of one
	// progress update; each retry means another chunk updated it first.
	manifestAttempts = 64
)

// uploadIDPattern is what a client's upload identifier may look like: the
// UUIDs and slugs the libraries generate, and nothing that could become a
// path.
var uploadIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// manifestName is the object recording an upload's progress, beside its
// chunks: updated by compare-and-swap after every chunk, so progress is one
// small read and write per chunk, never a listing.
const manifestName = "manifest.json"

// chunkManifest is an upload's progress.
type chunkManifest struct {
	// Total and Size are what the first chunk declared; later chunks must
	// declare the same.
	Total int   `json:"total"`
	Size  int64 `json:"size"`
	// Sizes maps each stored chunk's index to its size.
	Sizes map[int]int64 `json:"sizes"`
	// AssemblingSince is when the current assembler last proved it is
	// alive (elected, or renewed its claim before a step); zero while
	// chunks are still arriving.
	AssemblingSince time.Time `json:"assembling_since,omitzero"`
	// Generation counts assembler elections. An assembler holds the
	// generation it was elected with and renews its claim against it
	// before each step that changes something (settling the file, which
	// OnStored follows, and removing the chunks); a takeover increments
	// it, so a superseded assembler finds another generation and stops.
	Generation int64 `json:"generation,omitempty"`
}

// ownerIndex lists the uploads one owner has in progress, so the
// MaxUploadsPerOwner check and the admission of a new upload are one
// compare-and-swap rather than a listing followed by a write.
type ownerIndex struct {
	// IDs are the client upload IDs admitted and not yet assembled.
	IDs []string `json:"ids"`
}

// staged is the bytes of the stored chunks other than skip.
func (m *chunkManifest) staged(skip int) int64 {
	var n int64
	for i, size := range m.Sizes {
		if i != skip {
			n += size
		}
	}
	return n
}

// ChunkedOptions configure Chunked.
type ChunkedOptions struct {
	// Staging is the directory where chunks wait until the file is
	// complete. Required, not the root. Sweep it (upload.Sweep, or the
	// jobs.UploadSweep job) for chunks of uploads nobody finished.
	Staging string
	// Owner names who sends a request (a user or tenant ID), so one
	// client can never add chunks to, or complete, another's upload.
	// Required: a deployment without users returns a constant.
	Owner func(r *http.Request) (string, error)
	// Fields are the client library's field names; the zero value means
	// DropzoneFields.
	Fields ChunkFields
	// MaxChunkBytes caps each chunk; 0 means DefaultMaxChunkBytes. A
	// chunk is also capped by what is left of the file's declared size,
	// so the chunks of one upload can never stage more than the file.
	MaxChunkBytes int64
	// MaxUploadsPerOwner caps the uploads one owner may have in progress
	// at once; 0 means DefaultMaxUploadsPerOwner. Starting one more is
	// CodeQuotaExceeded, so a client cannot stage without bound by
	// opening upload after upload. The count is exact under concurrency:
	// uploads are admitted into a small per-owner index by
	// compare-and-swap. An upload leaves it when it is assembled (or its
	// assembly fails); an abandoned one stops counting once Sweep has
	// removed its manifest, checked only when the owner is at the cap.
	MaxUploadsPerOwner int
	// AssemblyTimeout is how long an assembler may go without renewing
	// its claim before another request may take over (the first one
	// crashed or stalled); 0 means DefaultAssemblyTimeout. The assembler
	// renews its claim before settling the file (just before OnStored)
	// and before removing the chunks, and stops when it was taken over, so the
	// file is stored and reported once. Streaming the file is one step:
	// a stalled stream is the case a takeover exists for.
	AssemblyTimeout time.Duration
	// StagingDisk holds the chunks; nil means the destination disk. It
	// needs CompareAndSwap and ConditionalWrite, which keep an upload's
	// progress consistent across concurrent chunks and instances: point
	// it at a local or memory disk when the destination lacks them.
	StagingDisk *filesystem.Disk
	// Resolve gives each request its own policy; see Resolver. It must
	// return the same policy for every chunk of one file.
	Resolve Resolver
	// OnStored runs once the file is assembled and stored, typically to
	// create its database row; an error deletes the file and fails the
	// request.
	OnStored func(ctx context.Context, r *http.Request, file Result) error
	// Now is the clock assembly claims are stamped and judged by against
	// AssemblyTimeout; nil means time.Now. Every instance serving one
	// staging directory should share a clock, or a skewed one takes over
	// an assembly early or late. A fake clock makes takeover testable
	// without sleeping.
	Now func() time.Time
	// Errors renders every failed chunk request; see fshttp.ErrorWriter.
	// nil is JSONErrors.
	Errors fshttp.ErrorWriter
	// Respond renders every successful chunk request (a Response for the
	// chunk that completes the file, a ChunkProgress for the others); see
	// SuccessWriter. nil is JSONSuccess, today's bodies unchanged.
	Respond SuccessWriter
}

// ChunkProgress is the body Chunked answers a chunk that did not complete
// its file with: how many of the file's chunks have arrived, of how many.
type ChunkProgress struct {
	Received int `json:"received"`
	Total    int `json:"total"`
}

// Chunked accepts files sent in chunks by Dropzone, FilePond, resumable.js
// and similar libraries: each POST carries one chunk as multipart form
// data, and chunks may arrive in any order and be retried. Chunks wait in
// opts.Staging, with their progress in a small manifest updated by
// compare-and-swap; the request that completes the set assembles them in order
// and stores the file through Receive, so the policy applies to the whole
// file exactly as for a single upload (detected type, size, name rules,
// Process, Inspect, Settle), then answers 201 with Response{Files}. Other
// chunks answer 200 with how many have arrived.
//
// A first chunk whose type the policy refuses is refused at once; a
// declared total size over the policy's MaxSize, chunks that would stage
// more than the declared size, or that do not add up to it, are refused
// too, as is a new upload past the owner's MaxUploadsPerOwner.
//
// The options are checked here, once: a Staging that is the root or not
// a valid directory, a nil Owner, or a staging disk without
// ConditionalWrite and CompareAndSwap is CodeInvalidArgument, so a
// misconfigured endpoint fails at startup rather than on every chunk.
func Chunked(disk *filesystem.Disk, base Policy, opts ChunkedOptions) (http.Handler, error) {
	if opts.Fields == (ChunkFields{}) {
		opts.Fields = DropzoneFields()
	}
	if opts.MaxChunkBytes <= 0 {
		opts.MaxChunkBytes = DefaultMaxChunkBytes
	}
	if opts.MaxUploadsPerOwner <= 0 {
		opts.MaxUploadsPerOwner = DefaultMaxUploadsPerOwner
	}
	if opts.AssemblyTimeout <= 0 {
		opts.AssemblyTimeout = DefaultAssemblyTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	staging := opts.StagingDisk
	if staging == nil {
		staging = disk
	}
	dir, err := filesystem.CleanDir(opts.Staging)
	switch caps := staging.Capabilities(); {
	case err != nil || dir == "" || opts.Owner == nil:
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "chunked uploads need a staging directory other than the root and an Owner")
	case !caps.ConditionalWrite || !caps.CompareAndSwap:
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "staging disk %s needs ConditionalWrite and CompareAndSwap to track chunks; set StagingDisk to one that has them", staging.Name())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSONError(opts.Errors, w, r, http.StatusMethodNotAllowed, filesystem.Errorf(filesystem.CodeInvalidArgument, "chunks must be POSTed"))
			return
		}
		p, err := resolve(opts.Resolve, r, base)
		if err != nil {
			writeResolveError(opts.Errors, w, r, err)
			return
		}
		owner, err := opts.Owner(r)
		if err != nil {
			writeJSONError(opts.Errors, w, r, statusOf(err), err)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, opts.MaxChunkBytes+formOverheadBytes)
		c := &chunkRequest{disk: disk, stage: staging, policy: p, opts: opts, staging: dir, owner: owner}
		out, err := c.serve(r)
		switch {
		case err != nil:
			writeJSONError(opts.Errors, w, r, statusOf(err), bodyError(err))
		case out.file != nil:
			writeSuccess(opts.Respond, w, r, http.StatusCreated, Response{Files: []Result{*out.file}})
		default:
			writeSuccess(opts.Respond, w, r, http.StatusOK, out.progress)
		}
	}), nil
}

// chunkOutcome is what one chunk request achieved: the stored file when it
// completed the set, otherwise the progress so far.
type chunkOutcome struct {
	file     *Result
	progress ChunkProgress
}

// chunkRequest is one chunk being handled.
type chunkRequest struct {
	disk    *filesystem.Disk
	stage   *filesystem.Disk
	policy  Policy
	opts    ChunkedOptions
	staging string
	owner   string
	id      string
	index   int
	total   int
	size    int64
}

// serve reads the chunk's fields and bytes, stores the chunk, and
// assembles the file when this chunk completes it.
func (c *chunkRequest) serve(r *http.Request) (chunkOutcome, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return chunkOutcome{}, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	fields := map[string]string{}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return chunkOutcome{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "no chunk in field %q", c.opts.Fields.File)
		}
		if err != nil {
			return chunkOutcome{}, err
		}
		if part.FormName() != c.opts.Fields.File {
			v, err := io.ReadAll(io.LimitReader(part, maxChunkFieldBytes))
			_ = part.Close()
			if err != nil {
				return chunkOutcome{}, err
			}
			fields[part.FormName()] = string(v)
			continue
		}
		defer func() { _ = part.Close() }()
		if err := c.parse(fields); err != nil {
			return chunkOutcome{}, err
		}
		return c.store(r, part)
	}
}

// maxChunkFieldBytes bounds a text field of a chunk request.
const maxChunkFieldBytes = 1 << 10

// parse validates the chunk's fields against the policy.
func (c *chunkRequest) parse(fields map[string]string) error {
	f := c.opts.Fields
	c.id = fields[f.UploadID]
	index, errIndex := strconv.Atoi(fields[f.Index])
	total, errTotal := strconv.Atoi(fields[f.Total])
	size, errSize := strconv.ParseInt(fields[f.TotalSize], 10, 64)
	c.index, c.total, c.size = index-f.IndexBase, total, size
	switch {
	case !uploadIDPattern.MatchString(c.id):
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "upload ID must match %s", uploadIDPattern)
	case errIndex != nil || errTotal != nil || errSize != nil:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "chunk index, total and total size must be integers")
	case c.total < 1 || c.total > MaxChunks || c.index < 0 || c.index >= c.total:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "chunk %d of %d is out of range (at most %d chunks)", index, total, MaxChunks)
	case c.size > c.policy.MaxSize:
		return filesystem.Errorf(filesystem.CodeTooLarge, "declared size %d exceeds %d", c.size, c.policy.MaxSize)
	case c.size < max(c.policy.MinSize, 1):
		return filesystem.Errorf(filesystem.CodeRejected, "declared size %d is under the minimum", c.size)
	}
	return nil
}

// dir is where this upload's chunks wait: under the staging directory, a
// digest of the owner (any string, made key-safe and unguessable from
// another owner's ID) and the client's upload ID.
func (c *chunkRequest) dir() string { return filesystem.JoinKey(c.ownerDir(), c.id) }

// ownerDir holds every upload of this owner.
func (c *chunkRequest) ownerDir() string { return filesystem.JoinKey(c.staging, c.ownerDigest()) }

// indexKey is the owner's ownerIndex, beside (not inside) ownerDir so no
// upload ID can collide with it.
func (c *chunkRequest) indexKey() string {
	return filesystem.JoinKey(c.staging, c.ownerDigest()+indexSuffix)
}

func (c *chunkRequest) ownerDigest() string {
	sum := sha256.Sum256([]byte(c.owner))
	return hex.EncodeToString(sum[:ownerDigestBytes])
}

const (
	// ownerDigestBytes of the owner's SHA-256 name its staging directory.
	ownerDigestBytes = 16
	// indexSuffix names the owner's index after its digest.
	indexSuffix = ".json"
)

// store writes the chunk within what is left of the declared size,
// records it in the upload's manifest, and assembles the file when this
// chunk completes it.
func (c *chunkRequest) store(r *http.Request, part *multipart.Part) (chunkOutcome, error) {
	ctx := r.Context()
	var body io.Reader = part
	if c.index == 0 {
		head := make([]byte, sniff.HeadSize)
		n, err := io.ReadFull(part, head)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return chunkOutcome{}, err
		}
		if err := c.policy.Accepts(c.policy.detect(head[:n], SafeFilename(clientFilename(part)))); err != nil {
			return chunkOutcome{}, err
		}
		body = io.MultiReader(bytes.NewReader(head[:n]), part)
	}
	m, _, err := c.openManifest(ctx)
	if err != nil {
		return chunkOutcome{}, err
	}
	// Never more than the file has left: the chunks of one upload cannot
	// stage more than its declared size, which the policy caps.
	budget := min(c.opts.MaxChunkBytes, c.size-m.staged(c.index))
	if budget <= 0 {
		return chunkOutcome{}, filesystem.Errorf(filesystem.CodeTooLarge, "the chunks already add up to the declared %d bytes", c.size)
	}
	key := filesystem.JoinKey(c.dir(), strconv.Itoa(c.index))
	e, err := c.stage.Write(ctx, key, body, filesystem.WriteOptions{MaxSize: budget})
	if err != nil {
		return chunkOutcome{}, err
	}
	m, assemble, err := c.record(ctx, e.Size)
	if err != nil {
		return chunkOutcome{}, err
	}
	progress := ChunkProgress{Received: len(m.Sizes), Total: c.total}
	if !assemble {
		return chunkOutcome{progress: progress}, nil
	}
	res, err := c.assemble(r, clientFilename(part), m.Generation)
	if err != nil {
		return chunkOutcome{}, err
	}
	return chunkOutcome{file: &res}, nil
}

// openManifest loads the upload's manifest with its ETag, creating it for
// a new upload and then admitting the upload into the owner's index (a
// refused upload's manifest is removed again). A chunk that declares
// another total or size than the upload's first is CodeInvalidArgument.
func (c *chunkRequest) openManifest(ctx context.Context) (chunkManifest, string, error) {
	for range manifestAttempts {
		m, etag, err := c.loadManifest(ctx)
		switch {
		case err == nil:
			if m.Total != c.total || m.Size != c.size {
				return chunkManifest{}, "", filesystem.Errorf(filesystem.CodeInvalidArgument, "the chunk declares %d chunks and %d bytes, the upload %d and %d", c.total, c.size, m.Total, m.Size)
			}
			return m, etag, nil
		case filesystem.CodeOf(err) != filesystem.CodeNotFound:
			return chunkManifest{}, "", err
		}
		// Created here or by a concurrent first chunk: reload it with its
		// ETag either way. The manifest exists before the upload is
		// admitted, so an admitted upload without one has been swept.
		fresh := chunkManifest{Total: c.total, Size: c.size, Sizes: map[int]int64{}}
		switch err := saveJSON(ctx, c.stage, c.manifestKey(), fresh, ""); filesystem.CodeOf(err) {
		case "":
			if err := c.admit(ctx); err != nil {
				_ = c.stage.DeletePrefix(context.WithoutCancel(ctx), c.dir())
				return chunkManifest{}, "", err
			}
		case filesystem.CodeExists:
		default:
			return chunkManifest{}, "", err
		}
	}
	return chunkManifest{}, "", errProgressChanging
}

// errProgressChanging is a compare-and-swap loop that kept losing to
// concurrent writers for manifestAttempts tries.
var errProgressChanging = filesystem.Errorf(filesystem.CodeUnavailable, "the upload's progress kept changing")

// admit adds this upload to the owner's index, refusing it with
// CodeQuotaExceeded when the owner already has MaxUploadsPerOwner in
// progress. At the cap it first drops uploads whose manifest is gone
// (swept as abandoned, or assembled by a request that failed to leave the
// index), so only uploads that can still complete count.
func (c *chunkRequest) admit(ctx context.Context) error {
	return c.updateIndex(ctx, func(ix *ownerIndex) (bool, error) {
		if slices.Contains(ix.IDs, c.id) {
			return false, nil
		}
		if len(ix.IDs) >= c.opts.MaxUploadsPerOwner {
			if err := c.prune(ctx, ix); err != nil {
				return false, err
			}
		}
		if len(ix.IDs) >= c.opts.MaxUploadsPerOwner {
			return false, filesystem.Errorf(filesystem.CodeQuotaExceeded, "%d chunked uploads are already in progress", len(ix.IDs))
		}
		ix.IDs = append(ix.IDs, c.id)
		return true, nil
	})
}

// prune drops the uploads of ix whose manifest no longer exists.
func (c *chunkRequest) prune(ctx context.Context, ix *ownerIndex) error {
	live := ix.IDs[:0]
	for _, id := range ix.IDs {
		_, err := c.stage.Stat(ctx, filesystem.JoinKey(c.ownerDir(), id, manifestName))
		switch filesystem.CodeOf(err) {
		case "":
			live = append(live, id)
		case filesystem.CodeNotFound:
		default:
			return err
		}
	}
	ix.IDs = live
	return nil
}

// release removes this upload from the owner's index once it is
// assembled or its assembly failed.
func (c *chunkRequest) release(ctx context.Context) error {
	return c.updateIndex(ctx, func(ix *ownerIndex) (bool, error) {
		n := len(ix.IDs)
		ix.IDs = slices.DeleteFunc(ix.IDs, func(id string) bool { return id == c.id })
		return len(ix.IDs) != n, nil
	})
}

// updateIndex applies change to the owner's index by compare-and-swap,
// retrying when another request updated it first; change reports whether
// it changed anything. An empty index is deleted, so a finished owner
// leaves nothing in staging; a missing one (a first upload, or one Sweep
// removed) is rebuilt from the owner's upload directories.
func (c *chunkRequest) updateIndex(ctx context.Context, change func(*ownerIndex) (bool, error)) error {
	for range manifestAttempts {
		var ix ownerIndex
		etag, err := loadJSON(ctx, c.stage, c.indexKey(), &ix)
		if filesystem.CodeOf(err) == filesystem.CodeNotFound {
			ix, err = c.seedIndex(ctx)
		}
		if err != nil {
			return err
		}
		changed, err := change(&ix)
		switch {
		case err != nil:
			return err
		case len(ix.IDs) == 0 && etag == "":
			return nil
		case len(ix.IDs) == 0:
			// Not conditional, and need not be: an upload admitted in
			// between is found again when the index is rebuilt from the
			// directories, because its manifest exists before admission.
			return c.stage.Delete(ctx, c.indexKey())
		case !changed && etag != "":
			// A seeded index is saved even unchanged: it already counts
			// this upload, whose manifest the listing saw.
			return nil
		}
		switch err := saveJSON(ctx, c.stage, c.indexKey(), ix, etag); filesystem.CodeOf(err) {
		case "":
			return nil
		case filesystem.CodeConflict, filesystem.CodeExists:
			continue
		default:
			return err
		}
	}
	return errProgressChanging
}

// seedIndex lists the owner's upload directories, for an index that does
// not exist yet; creating it is conditional, so concurrent seeds agree.
func (c *chunkRequest) seedIndex(ctx context.Context) (ownerIndex, error) {
	ix := ownerIndex{IDs: []string{}}
	for e, err := range c.stage.List(ctx, c.ownerDir(), filesystem.ListOptions{}) {
		if err != nil {
			return ownerIndex{}, err
		}
		if e.IsDir {
			ix.IDs = append(ix.IDs, path.Base(e.Key))
		}
	}
	return ix, nil
}

// record adds the stored chunk to the manifest by compare-and-swap,
// retrying when another chunk updated it first. It refuses (deleting the
// chunk) when concurrent chunks together passed the declared size, and
// elects this request to assemble when the set is complete and nobody is
// assembling (or the last assembler timed out).
func (c *chunkRequest) record(ctx context.Context, size int64) (chunkManifest, bool, error) {
	for range manifestAttempts {
		m, etag, err := c.loadManifest(ctx)
		if err != nil {
			return chunkManifest{}, false, err
		}
		m.Sizes[c.index] = size
		if m.staged(-1) > m.Size {
			_ = c.stage.Delete(context.WithoutCancel(ctx), filesystem.JoinKey(c.dir(), strconv.Itoa(c.index)))
			return chunkManifest{}, false, filesystem.Errorf(filesystem.CodeTooLarge, "the chunks add up to more than the declared %d bytes", m.Size)
		}
		now := c.opts.Now()
		assemble := len(m.Sizes) == m.Total && (m.AssemblingSince.IsZero() || now.Sub(m.AssemblingSince) > c.opts.AssemblyTimeout)
		if assemble {
			m.AssemblingSince = now
			m.Generation++
		}
		switch err := saveJSON(ctx, c.stage, c.manifestKey(), m, etag); filesystem.CodeOf(err) {
		case "":
			return m, assemble, nil
		case filesystem.CodeConflict:
			continue
		default:
			return chunkManifest{}, false, err
		}
	}
	return chunkManifest{}, false, errProgressChanging
}

func (c *chunkRequest) manifestKey() string { return filesystem.JoinKey(c.dir(), manifestName) }

// loadManifest reads the manifest and the ETag it was read at.
func (c *chunkRequest) loadManifest(ctx context.Context) (chunkManifest, string, error) {
	var m chunkManifest
	etag, err := loadJSON(ctx, c.stage, c.manifestKey(), &m)
	if err != nil {
		return chunkManifest{}, "", err
	}
	if m.Sizes == nil {
		m.Sizes = map[int]int64{}
	}
	return m, etag, nil
}

// stagingRecord is what Chunked keeps beside the chunks: plain structs of
// strings, ints, maps of ints and times, which always marshal.
type stagingRecord interface{ chunkManifest | ownerIndex }

// loadJSON decodes the staging object at key into v and returns the ETag
// it was read at.
func loadJSON[T stagingRecord](ctx context.Context, disk *filesystem.Disk, key string, v *T) (string, error) {
	rc, e, err := disk.ReadIf(ctx, key, filesystem.ReadOptions{})
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	if err := json.NewDecoder(rc).Decode(v); err != nil {
		return "", filesystem.NewError(filesystem.CodeUnknown, err)
	}
	return e.ETag, nil
}

// saveJSON writes v at key, creating it (etag "") or replacing the
// version read at etag; another writer in between is CodeConflict or
// CodeExists.
func saveJSON[T stagingRecord](ctx context.Context, disk *filesystem.Disk, key string, v T, etag string) error {
	data, _ := json.Marshal(v) // a stagingRecord always marshals
	_, err := disk.WriteBytes(ctx, key, data, filesystem.WriteOptions{IfNotExists: etag == "", IfMatch: etag})
	return err
}

// claim renews this assembler's lease on the upload before a step that
// changes something: it fails with CodeConflict when another request has
// taken the assembly over (a newer generation, or the manifest already
// gone with the finished upload), and otherwise restarts the
// AssemblyTimeout, so nobody can take over while the step runs.
func (c *chunkRequest) claim(ctx context.Context, generation int64) error {
	for range manifestAttempts {
		m, etag, err := c.loadManifest(ctx)
		switch {
		case filesystem.CodeOf(err) == filesystem.CodeNotFound, err == nil && m.Generation != generation:
			return errSuperseded
		case err != nil:
			return err
		}
		m.AssemblingSince = c.opts.Now()
		switch err := saveJSON(ctx, c.stage, c.manifestKey(), m, etag); filesystem.CodeOf(err) {
		case "":
			return nil
		case filesystem.CodeConflict:
			continue
		default:
			return err
		}
	}
	return errProgressChanging
}

// errSuperseded is an assembler that was taken over.
var errSuperseded = filesystem.Errorf(filesystem.CodeConflict, "another request took over assembling this upload")

// assemble streams the chunks in order through Receive, checks they add
// up to the declared size, runs OnStored, and removes the chunks. On any
// failure the chunks are removed too: the client starts the file again.
// Every step that changes something is fenced by claim with the
// generation this request was elected with; a superseded assembler
// deletes only the file it stored, never the chunks its successor reads.
func (c *chunkRequest) assemble(r *http.Request, filename string, generation int64) (Result, error) {
	res, err := c.storeWhole(r, filename, generation)
	cleanup := context.WithoutCancel(r.Context())
	if c.claim(cleanup, generation) == nil {
		_ = c.stage.DeletePrefix(cleanup, c.dir())
		_ = c.release(cleanup)
	}
	return res, err
}

// storeWhole stores the assembled file and reports it to OnStored.
func (c *chunkRequest) storeWhole(r *http.Request, filename string, generation int64) (Result, error) {
	ctx := r.Context()
	// The claim before settling also covers OnStored, which follows at
	// once: nobody can take over within AssemblyTimeout of it.
	p := c.policy
	settle := p.Settle
	p.Settle = func(ctx context.Context, disk *filesystem.Disk, res Result) (Result, error) {
		if err := c.claim(ctx, generation); err != nil {
			return Result{}, err
		}
		if settle == nil {
			return res, nil
		}
		return settle(ctx, disk, res)
	}
	whole := &chunkStream{ctx: ctx, disk: c.stage, dir: c.dir(), total: c.total}
	res, err := Receive(ctx, c.disk, whole, filename, p)
	if err != nil {
		return Result{}, err
	}
	if res.Size != c.size {
		return Result{}, reject(ctx, c.disk, res.Key, filesystem.Errorf(filesystem.CodeInvalidArgument, "the chunks add up to %d bytes, not the declared %d", res.Size, c.size))
	}
	if c.opts.OnStored != nil {
		if err := c.opts.OnStored(ctx, r, res); err != nil {
			return Result{}, reject(ctx, c.disk, res.Key, err)
		}
	}
	return res, nil
}

// chunkStream reads the stored chunks in order, opening each only when
// the previous one is exhausted.
type chunkStream struct {
	ctx   context.Context
	disk  *filesystem.Disk
	dir   string
	total int
	next  int
	cur   io.ReadCloser
}

func (s *chunkStream) Read(p []byte) (int, error) {
	for {
		if s.cur == nil {
			if s.next == s.total {
				return 0, io.EOF
			}
			rc, err := s.disk.Read(s.ctx, filesystem.JoinKey(s.dir, strconv.Itoa(s.next)))
			if err != nil {
				return 0, err
			}
			s.cur, s.next = rc, s.next+1
		}
		n, err := s.cur.Read(p)
		if errors.Is(err, io.EOF) {
			_ = s.cur.Close()
			s.cur = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

// clientFilename is the file name the client sent for part, with any
// directories it included (multipart.Part.FileName keeps only the base),
// for SafePath.
//
// Callers have already matched part.FormName and part.FileName, which
// parse the same header, so it parses here too and names a file.
func clientFilename(part *multipart.Part) string {
	_, params, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	return params["filename"]
}
