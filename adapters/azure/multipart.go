package azure

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"

	"github.com/ubgo/filesystem"
)

// MultipartDir is the reserved prefix holding one marker blob per
// unfinished multipart upload. Listings skip it and keys inside it are
// refused.
const MultipartDir = ".filesystem-multipart"

// uploadIDPattern is the shape of upload IDs this driver issues.
var uploadIDPattern = regexp.MustCompile(`^[a-z2-7]{26,}$`)

// uploadMeta is what CreateMultipart records for CompleteMultipart.
type uploadMeta struct {
	Key         string                 `json:"key"`
	ContentType string                 `json:"content_type,omitempty"`
	Metadata    map[string]string      `json:"metadata,omitempty"`
	Headers     filesystem.HTTPHeaders `json:"headers,omitzero"`
	IfNotExists bool                   `json:"if_not_exists,omitempty"`
	// Request is the RequestOptions CreateMultipart's context carried for
	// the key, kept so every part is staged with the same encryption scope
	// the completion commits with, whatever context those calls get.
	Request RequestOptions `json:"request,omitzero"`
}

// markerExt ends every marker blob's name. markerKey writes it and
// scanAbandoned strips it to recover the upload ID, so the two must share
// this one value: a marker named any other way is never swept.
const markerExt = ".json"

func markerKey(id string) string { return MultipartDir + "/" + id + markerExt }

func reserved(key string) bool {
	return key == MultipartDir || strings.HasPrefix(key, MultipartDir+"/")
}

func checkKey(key string) error {
	if reserved(key) {
		return filesystem.Errorf(filesystem.CodeInvalidKey, "azure: keys under %q are reserved", MultipartDir)
	}
	return nil
}

// blockID is the base64 block ID of part n of upload id. Every block ID of
// a blob must have the same length, hence the fixed-width number.
func blockID(id string, n int) string {
	return base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "%s-%05d", id, n))
}

func noUpload(id string) error {
	return filesystem.Errorf(filesystem.CodeNotFound, "azure: no multipart upload %q", id)
}

// loadUpload reads an upload's marker and checks it belongs to key.
func (d *Driver) loadUpload(ctx context.Context, key, id string) (uploadMeta, error) {
	var m uploadMeta
	if !uploadIDPattern.MatchString(id) {
		return m, noUpload(id)
	}
	resp, err := d.container.NewBlobClient(markerKey(id)).DownloadStream(ctx, nil)
	if err != nil {
		if filesystem.CodeOf(mapErr(err)) == filesystem.CodeNotFound {
			return m, noUpload(id)
		}
		return m, mapErr(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return m, err
	}
	if m.Key != key {
		return m, noUpload(id)
	}
	return m, nil
}

// CreateMultipart implements filesystem.MultipartUploader.
func (d *Driver) CreateMultipart(ctx context.Context, key string, opts filesystem.WriteOptions) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	ro := requestOptions(ctx, key)
	if _, err := d.settings(key, ro); err != nil {
		return "", err
	}
	id := strings.ToLower(rand.Text())
	// Marshalling strings, bools, a map of strings and a time cannot fail.
	meta, _ := json.Marshal(uploadMeta{Key: key, ContentType: opts.ContentType, Metadata: opts.Metadata, Headers: opts.Headers, IfNotExists: opts.IfNotExists, Request: ro})
	if _, err := d.container.NewBlockBlobClient(markerKey(id)).UploadBuffer(ctx, meta, nil); err != nil {
		return "", mapErr(err)
	}
	return id, nil
}

// UploadPart implements filesystem.MultipartUploader by staging a block.
// Azure needs a seekable body to retry a block, so a body that cannot seek
// (including an *os.File that is a pipe, which has Seek but fails it) is
// read into memory first. A seekable body is staged from its current
// position.
func (d *Driver) UploadPart(ctx context.Context, key, uploadID string, number int, body io.Reader, _ int64) (filesystem.Part, error) {
	m, err := d.loadUpload(ctx, key, uploadID)
	if err != nil {
		return filesystem.Part{}, err
	}
	ws, err := d.settings(key, m.Request)
	if err != nil {
		return filesystem.Part{}, err
	}
	rs, size, err := seekableSection(body)
	if err != nil {
		return filesystem.Part{}, err
	}
	id := blockID(uploadID, number)
	if _, err := d.container.NewBlockBlobClient(key).StageBlock(ctx, id, streaming.NopCloser(rs), &blockblob.StageBlockOptions{CPKScopeInfo: ws.cpkScope(), CPKInfo: d.cpk()}); err != nil {
		return filesystem.Part{}, mapErr(err)
	}
	return filesystem.Part{Number: number, ETag: id, Size: size}, nil
}

// seekableSection returns body as a ReadSeeker whose offset 0 is the
// body's current position (the SDK seeks to 0 to retry a block), and how
// many bytes remain. A body whose Seek fails, such as a pipe, is read into
// memory instead.
func seekableSection(body io.Reader) (io.ReadSeeker, int64, error) {
	if rs, ok := body.(io.ReadSeeker); ok {
		if start, err := rs.Seek(0, io.SeekCurrent); err == nil {
			end, err := rs.Seek(0, io.SeekEnd)
			if err == nil {
				_, err = rs.Seek(start, io.SeekStart)
			}
			if err != nil {
				return nil, 0, err
			}
			return &shifted{rs: rs, base: start}, end - start, nil
		}
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, 0, err
	}
	return bytes.NewReader(data), int64(len(data)), nil
}

// shifted is a ReadSeeker whose offsets count from base.
type shifted struct {
	rs   io.ReadSeeker
	base int64
}

func (s *shifted) Read(p []byte) (int, error) { return s.rs.Read(p) }

func (s *shifted) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekStart {
		offset += s.base
	}
	pos, err := s.rs.Seek(offset, whence)
	return pos - s.base, err
}

// ListParts implements filesystem.MultipartUploader from the blob's
// uncommitted blocks.
func (d *Driver) ListParts(ctx context.Context, key, uploadID string) ([]filesystem.Part, error) {
	if _, err := d.loadUpload(ctx, key, uploadID); err != nil {
		return nil, err
	}
	resp, err := d.container.NewBlockBlobClient(key).GetBlockList(ctx, blockblob.BlockListTypeUncommitted, nil)
	if err != nil {
		if filesystem.CodeOf(mapErr(err)) == filesystem.CodeNotFound {
			return nil, nil // nothing staged yet
		}
		return nil, mapErr(err)
	}
	var parts []filesystem.Part
	for _, b := range resp.UncommittedBlocks {
		// Block names are always base64 (the API requires it); a block from
		// another writer that does not parse as ours is skipped below.
		raw, _ := base64.StdEncoding.DecodeString(val(b.Name))
		var id string
		var n int
		if _, err := fmt.Sscanf(strings.Replace(string(raw), "-", " ", 1), "%s %d", &id, &n); err != nil || id != uploadID {
			continue
		}
		parts = append(parts, filesystem.Part{Number: n, ETag: val(b.Name), Size: val(b.Size)})
	}
	return parts, nil
}

// CompleteMultipart implements filesystem.MultipartUploader by committing
// the listed blocks in order, with the options recorded at creation.
func (d *Driver) CompleteMultipart(ctx context.Context, key, uploadID string, parts []filesystem.Part) (filesystem.Entry, error) {
	m, err := d.loadUpload(ctx, key, uploadID)
	if err != nil {
		return filesystem.Entry{}, err
	}
	ids := make([]string, 0, len(parts))
	prev := 0
	for _, p := range parts {
		id := blockID(uploadID, p.Number)
		if p.Number <= prev || p.ETag != id {
			return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "part %d is out of order or has the wrong ETag", p.Number)
		}
		prev = p.Number
		ids = append(ids, id)
	}
	ws, err := d.settings(key, m.Request)
	if err != nil {
		return filesystem.Entry{}, err
	}
	o := &blockblob.CommitBlockListOptions{Metadata: toAzureMetadata(m.Metadata), HTTPHeaders: blobHeaders(m.ContentType, m.Headers), CPKScopeInfo: ws.cpkScope(), CPKInfo: d.cpk(), Tier: ws.tier, Tags: ws.tags}
	if m.IfNotExists {
		o.AccessConditions = &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: to.Ptr(azcore.ETagAny)}}
	}
	if _, err := d.container.NewBlockBlobClient(key).CommitBlockList(ctx, ids, o); err != nil {
		return filesystem.Entry{}, mapErr(err)
	}
	_ = d.deleteBlob(ctx, markerKey(uploadID))
	if err := d.retain(ctx, key, ws); err != nil {
		return filesystem.Entry{}, err
	}
	return d.Stat(ctx, key)
}

// AbortMultipart implements filesystem.MultipartUploader. It removes the
// upload's marker; Azure discards the staged blocks itself after seven days.
func (d *Driver) AbortMultipart(ctx context.Context, _ string, uploadID string) error {
	if !uploadIDPattern.MatchString(uploadID) {
		return nil
	}
	return d.deleteBlob(ctx, markerKey(uploadID))
}

// SweepAbandoned implements filesystem.AbandonedSweeper for multipart
// uploads created before before. A marker records only when its upload
// started (parts are staged on the target blob, not the marker), so that
// is the time compared. For each such upload it first discards the staged
// blocks where that is safe (see discardBlocks), then deletes the marker;
// without its marker an upload is unknown, and UploadPart, ListParts and
// CompleteMultipart answer CodeNotFound. Blobs that are not markers this
// driver issued are left alone, and a marker removed by a concurrent
// Complete or Abort is not counted. Blocks are handled before the marker
// so that a sweep failing part-way can be rerun.
func (d *Driver) SweepAbandoned(ctx context.Context, before time.Time) (int, error) {
	n := 0
	err := d.scanAbandoned(ctx, before, func(id string, _ time.Time) error {
		if err := d.discardBlocks(ctx, id); err != nil {
			return err
		}
		_, err := d.container.NewBlobClient(markerKey(id)).Delete(ctx, nil)
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return nil
		}
		if err != nil {
			return mapErr(err)
		}
		n++
		return nil
	})
	return n, err
}

// sweepMetadataKey is the metadata name on the empty blob the sweep commits
// to discard an upload's blocks; its value is the upload ID. It is what
// lets a later sweep tell that leftover from a user's own empty blob.
const sweepMetadataKey = "filesystemsweep"

// sweepDeleteTimeout bounds deleting the empty blob the sweep committed.
// The delete runs even when the caller's context has ended, because
// skipping it would leave that blob visible at a user's key.
const sweepDeleteTimeout = 30 * time.Second

// discardBlocks removes the uncommitted blocks of abandoned upload id when
// that cannot touch anything else. Azure has no call to delete an
// uncommitted block, but committing a block list discards every block not
// in it, so for a target blob that does not exist yet the driver commits
// an empty list (If-None-Match: *, so an existing blob is never replaced,
// tagged with sweepMetadataKey) and deletes the empty blob it made
// (If-Match its ETag). Until that delete, the empty blob is visible at the
// key; if the delete fails, the sweep returns the error and keeps the
// marker, and the next sweep finds the tagged empty blob and deletes it.
// It does nothing, leaving the blocks to Azure's seven-day expiry, when
// the blob exists (committing would rewrite it) or when any uncommitted
// block belongs to someone else (another upload, or a Write still
// staging), whose blocks the commit would destroy. A marker that is gone
// or unreadable is skipped.
func (d *Driver) discardBlocks(ctx context.Context, id string) error {
	m, err := d.readMarker(ctx, id)
	if err != nil || m.Key == "" || reserved(m.Key) {
		return nil
	}
	bb := d.container.NewBlockBlobClient(m.Key)
	list, err := bb.GetBlockList(ctx, blockblob.BlockListTypeAll, nil)
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return nil // nothing staged
	}
	if err != nil {
		return mapErr(err)
	}
	if len(list.CommittedBlocks) == 0 && len(list.UncommittedBlocks) == 0 {
		return d.deleteLeftover(ctx, bb, id)
	}
	if len(list.CommittedBlocks) > 0 {
		return nil
	}
	for _, b := range list.UncommittedBlocks {
		raw, _ := base64.StdEncoding.DecodeString(val(b.Name))
		if !strings.HasPrefix(string(raw), id+"-") {
			return nil
		}
	}
	o := &blockblob.CommitBlockListOptions{
		Metadata:         map[string]*string{sweepMetadataKey: to.Ptr(id)},
		AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: to.Ptr(azcore.ETagAny)}},
	}
	resp, err := bb.CommitBlockList(ctx, nil, o)
	if code := filesystem.CodeOf(mapErr(err)); err != nil && code == filesystem.CodeExists {
		return nil // created meanwhile: not ours to touch
	}
	if err != nil {
		return mapErr(err)
	}
	return deleteIfMatch(ctx, bb, resp.ETag)
}

// deleteLeftover deletes the blob at bb when it is the empty blob an
// earlier sweep of upload id committed and failed to delete: no blocks,
// no bytes, and tagged with id. A user's own empty blob carries no tag and
// is kept.
func (d *Driver) deleteLeftover(ctx context.Context, bb *blockblob.Client, id string) error {
	p, err := bb.GetProperties(ctx, nil)
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return nil
	}
	if err != nil {
		return mapErr(err)
	}
	if val(p.ContentLength) != 0 || metadata(p.Metadata)[sweepMetadataKey] != id {
		return nil
	}
	return deleteIfMatch(ctx, bb, p.ETag)
}

// deleteIfMatch deletes the blob if it is still the version etag names.
// It runs on a context detached from ctx's cancellation (bounded by
// sweepDeleteTimeout), so a sweep cancelled between its commit and this
// delete still removes the blob it made visible.
func deleteIfMatch(ctx context.Context, bb *blockblob.Client, etag *azcore.ETag) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sweepDeleteTimeout)
	defer cancel()
	_, err := bb.Delete(ctx, &blob.DeleteOptions{AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: etag}}})
	if err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ConditionNotMet) {
		return mapErr(err)
	}
	return nil
}

// readMarker reads upload id's marker without checking its key.
func (d *Driver) readMarker(ctx context.Context, id string) (uploadMeta, error) {
	var m uploadMeta
	resp, err := d.container.NewBlobClient(markerKey(id)).DownloadStream(ctx, nil)
	if err != nil {
		return m, mapErr(err)
	}
	defer func() { _ = resp.Body.Close() }()
	err = json.NewDecoder(resp.Body).Decode(&m)
	return m, err
}
