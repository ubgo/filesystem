package azure

import (
	"cmp"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/ubgo/filesystem"
)

func etag(e *azcore.ETag) string {
	if e == nil {
		return ""
	}
	return strings.Trim(string(*e), `"`)
}

// metadata converts Azure's pointer-valued metadata.
func metadata(m map[string]*string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = val(v) // Azure may change the case of names
	}
	return out
}

func toAzureMetadata(m map[string]string) map[string]*string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]*string, len(m))
	for k, v := range m {
		out[k] = to.Ptr(v)
	}
	return out
}

// Stat implements filesystem.Driver.
func (d *Driver) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	if err := checkKey(key); err != nil {
		return filesystem.Entry{}, err
	}
	p, err := d.properties(ctx, key)
	if err != nil {
		return filesystem.Entry{}, err
	}
	return filesystem.Entry{
		Key:          key,
		Size:         val(p.ContentLength),
		ModTime:      val(p.LastModified),
		ContentType:  val(p.ContentType),
		ETag:         etag(p.ETag),
		Metadata:     metadata(p.Metadata),
		StorageClass: val(p.AccessTier),
		Headers: filesystem.HTTPHeaders{
			CacheControl:       val(p.CacheControl),
			ContentDisposition: val(p.ContentDisposition),
			ContentEncoding:    val(p.ContentEncoding),
		},
	}, nil
}

// properties reads key's properties, presenting Config.CustomerKey.
func (d *Driver) properties(ctx context.Context, key string) (blob.GetPropertiesResponse, error) {
	p, err := d.container.NewBlobClient(key).GetProperties(ctx, &blob.GetPropertiesOptions{CPKInfo: d.cpk()})
	return p, mapErr(err)
}

// blobHeaders converts a content type and HTTP headers to Azure's blob
// headers, or nil when none is set.
func blobHeaders(contentType string, h filesystem.HTTPHeaders) *blob.HTTPHeaders {
	if contentType == "" && h.IsZero() {
		return nil
	}
	optional := func(v string) *string {
		if v == "" {
			return nil
		}
		return to.Ptr(v)
	}
	return &blob.HTTPHeaders{
		BlobContentType:        optional(contentType),
		BlobCacheControl:       optional(h.CacheControl),
		BlobContentDisposition: optional(h.ContentDisposition),
		BlobContentEncoding:    optional(h.ContentEncoding),
	}
}

// Read implements filesystem.Driver. A range starting past the end is an
// empty read.
func (d *Driver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	// An explicit Accept-Encoding stops Go's transport from adding its own
	// and then silently decompressing a blob stored with Content-Encoding
	// gzip: reads return the bytes as stored.
	ctx = policy.WithHTTPHeader(ctx, http.Header{"Accept-Encoding": {"identity"}})
	resp, err := d.container.NewBlobClient(key).DownloadStream(ctx, &blob.DownloadStreamOptions{Range: blob.HTTPRange{Offset: rng.Offset, Count: rng.Length}, CPKInfo: d.cpk()})
	if bloberror.HasCode(err, bloberror.InvalidRange) {
		// Azure answers InvalidRange only for a blob that exists (a missing
		// one is BlobNotFound), so this is the contract's empty read.
		return io.NopCloser(strings.NewReader("")), nil
	}
	if err != nil {
		return nil, mapErr(err)
	}
	return resp.Body, nil
}

// Write implements filesystem.Driver with a streaming block upload, which
// commits only after the whole body was staged, in blocks of
// Config.BlockSize staged Config.Concurrency at a time. IfMatch
// (compare-and-swap) conditions the commit on the ETag, so Azure refuses it atomically when
// the blob changed or vanished (CodeConflict). With Config.RetainFor the
// new blob then gets its immutability policy (see retain).
func (d *Driver) Write(ctx context.Context, key string, body io.Reader, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	if err := checkKey(key); err != nil {
		return filesystem.Entry{}, err
	}
	ws, err := d.settings(key, requestOptions(ctx, key))
	if err != nil {
		return filesystem.Entry{}, err
	}
	o := &blockblob.UploadStreamOptions{
		Metadata:     toAzureMetadata(opts.Metadata),
		HTTPHeaders:  blobHeaders(opts.ContentType, opts.Headers),
		CPKScopeInfo: ws.cpkScope(),
		CPKInfo:      d.cpk(),
		AccessTier:   ws.tier,
		Tags:         ws.tags,
		BlockSize:    d.blockSize(),
		Concurrency:  d.concurrency(),
	}
	if d.cfg.ValidateCRC64 {
		o.TransactionalValidation = blob.TransferValidationTypeComputeCRC64()
	}
	switch {
	case opts.IfNotExists:
		o.AccessConditions = &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: to.Ptr(azcore.ETagAny)}}
	case opts.IfMatch != "":
		o.AccessConditions = &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: to.Ptr(azcore.ETag(`"` + opts.IfMatch + `"`))}}
	}
	if _, err := d.container.NewBlockBlobClient(key).UploadStream(ctx, body, o); err != nil {
		if opts.IfMatch != "" && swapLost(err) {
			return filesystem.Entry{}, filesystem.NewError(filesystem.CodeConflict, err)
		}
		return filesystem.Entry{}, readOrMap(err)
	}
	if err := d.retain(ctx, key, ws); err != nil {
		return filesystem.Entry{}, err
	}
	return d.Stat(ctx, key)
}

// swapLost reports whether a conditional commit failed because another
// writer got there first: the ETag no longer matches, the blob is gone, or
// the other writer's commit discarded the blocks this one had staged
// (Azure drops uncommitted blocks on every commit), which surfaces as an
// invalid block list.
func swapLost(err error) bool {
	return bloberror.HasCode(err, bloberror.ConditionNotMet, bloberror.BlobNotFound, bloberror.InvalidBlockList)
}

// readOrMap keeps an error that came from the body (not from Azure)
// unclassified, so the Disk reports the body's own failure.
func readOrMap(err error) error {
	var resp *azcore.ResponseError
	if errors.As(err, &resp) {
		return mapErr(err)
	}
	return err
}

// Delete implements filesystem.Driver; deleting a missing blob succeeds.
func (d *Driver) Delete(ctx context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	return d.deleteBlob(ctx, key)
}

// deleteBlob deletes one blob, reserved or not, treating a missing blob as
// deleted. Delete calls it for objects, the multipart code for markers.
func (d *Driver) deleteBlob(ctx context.Context, key string) error {
	_, err := d.container.NewBlobClient(key).Delete(ctx, nil)
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return nil
	}
	return mapErr(err)
}

// List implements filesystem.Driver. The cursor is Azure's marker.
func (d *Driver) List(ctx context.Context, dir string, opts filesystem.ListOptions) (filesystem.Page, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = filesystem.DefaultPageSize
	}
	var prefix, marker *string
	if dir != "" {
		prefix = to.Ptr(dir + "/")
	}
	if opts.Cursor != "" {
		marker = to.Ptr(opts.Cursor)
	}
	var entries []filesystem.Entry
	var next *string
	if opts.Recursive {
		resp, err := d.container.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: prefix, Marker: marker, MaxResults: to.Ptr(int32(limit)), Include: container.ListBlobsInclude{Metadata: true}}).NextPage(ctx)
		if err != nil {
			return filesystem.Page{}, mapErr(err)
		}
		entries = d.items(resp.Segment.BlobItems)
		next = resp.NextMarker
	} else {
		resp, err := d.container.NewListBlobsHierarchyPager("/", &container.ListBlobsHierarchyOptions{Prefix: prefix, Marker: marker, MaxResults: to.Ptr(int32(limit)), Include: container.ListBlobsInclude{Metadata: true}}).NextPage(ctx)
		if err != nil {
			return filesystem.Page{}, mapErr(err)
		}
		entries = d.items(resp.Segment.BlobItems)
		for _, p := range resp.Segment.BlobPrefixes {
			if reserved(strings.TrimSuffix(val(p.Name), "/")) {
				continue
			}
			entries = append(entries, filesystem.Entry{Key: strings.TrimSuffix(val(p.Name), "/"), IsDir: true})
		}
		next = resp.NextMarker
	}
	slices.SortFunc(entries, func(a, b filesystem.Entry) int { return strings.Compare(a.Key, b.Key) })
	return filesystem.Page{Entries: entries, NextCursor: val(next)}, nil
}

func (d *Driver) items(items []*container.BlobItem) []filesystem.Entry {
	out := make([]filesystem.Entry, 0, len(items))
	for _, it := range items {
		name := val(it.Name)
		if strings.HasSuffix(name, "/") || reserved(name) {
			continue // a directory marker or an upload marker, not an object
		}
		e := filesystem.Entry{Key: name, Metadata: metadata(it.Metadata)}
		if p := it.Properties; p != nil {
			e.Size = val(p.ContentLength)
			e.ModTime = val(p.LastModified)
			e.ContentType = val(p.ContentType)
			e.ETag = etag(p.ETag)
			e.StorageClass = string(val(p.AccessTier))
		}
		out = append(out, e)
	}
	return out
}

// DefaultCopyPollInterval is Config.CopyPollInterval when zero: how often
// Copy checks a pending server-side copy.
const DefaultCopyPollInterval = 50 * time.Millisecond

// Copy implements filesystem.Copier with a server-side copy inside the
// account, waiting for it to finish (Azure copies are asynchronous).
func (d *Driver) Copy(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	if err := checkKey(dst); err != nil {
		return filesystem.Entry{}, err
	}
	if _, err := d.Stat(ctx, src); err != nil {
		return filesystem.Entry{}, err
	}
	return d.copyFrom(ctx, d.container.NewBlobClient(src).URL(), dst)
}

// copyFrom copies the blob at srcURL (in this account) to dst server-side,
// waits for the copy to finish and gives dst Config.RetainFor's policy, as
// every new blob gets. Copy and RestoreVersion both run through it. The
// call's RequestOptions for dst add tags, a tier, a hold or a retention;
// an encryption scope is refused, since the server-side copy cannot set
// one. Config.AccessTier is not applied to copies, as before
// RequestOptions existed.
func (d *Driver) copyFrom(ctx context.Context, srcURL, dst string) (filesystem.Entry, error) {
	ro := requestOptions(ctx, dst)
	if ro.EncryptionScope != "" {
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeUnsupported, "azure: a server-side copy cannot set an encryption scope")
	}
	ws, err := d.settings(dst, ro)
	if err != nil {
		return filesystem.Entry{}, err
	}
	co := &blob.StartCopyFromURLOptions{BlobTags: ws.tags}
	if ro.AccessTier != "" {
		co.Tier = ws.tier
	}
	if _, err := d.container.NewBlobClient(dst).StartCopyFromURL(ctx, srcURL, co); err != nil {
		return filesystem.Entry{}, mapErr(err)
	}
	for {
		p, err := d.container.NewBlobClient(dst).GetProperties(ctx, nil)
		if err != nil {
			return filesystem.Entry{}, mapErr(err)
		}
		switch status := val(p.CopyStatus); status {
		case blob.CopyStatusTypeSuccess, "":
			if err := d.retain(ctx, dst, ws); err != nil {
				return filesystem.Entry{}, err
			}
			return d.Stat(ctx, dst)
		case blob.CopyStatusTypePending:
		default:
			return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeUnknown, "azure: copy %s: %s", status, val(p.CopyStatusDescription))
		}
		select {
		case <-ctx.Done():
			return filesystem.Entry{}, filesystem.NewError(filesystem.CodeUnavailable, ctx.Err())
		case <-time.After(cmp.Or(d.cfg.CopyPollInterval, DefaultCopyPollInterval)):
		}
	}
}

// val dereferences an optional SDK field, giving the zero value for nil.
func val[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
