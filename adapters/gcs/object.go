package gcs

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/iterator"

	"github.com/ubgo/filesystem"
)

// aclPublicRead is the predefined ACL public objects get (the GCS JSON
// API's wire value); private ones get Driver.privateACL.
const aclPublicRead = "publicRead"

func (d *Driver) entry(a *storage.ObjectAttrs) filesystem.Entry {
	e := filesystem.Entry{
		Key:          a.Name,
		Size:         a.Size,
		ModTime:      a.Updated,
		ContentType:  a.ContentType,
		ETag:         strings.Trim(a.Etag, `"`),
		Metadata:     a.Metadata,
		StorageClass: a.StorageClass,
		Headers: filesystem.HTTPHeaders{
			CacheControl:       a.CacheControl,
			ContentDisposition: a.ContentDisposition,
			ContentEncoding:    a.ContentEncoding,
		},
	}
	if d.cfg.Visibility == VisibilityACL {
		e.Visibility = filesystem.VisibilityPrivate
		for _, r := range a.ACL {
			if r.Entity == storage.AllUsers && (r.Role == storage.RoleReader || r.Role == storage.RoleOwner) {
				e.Visibility = filesystem.VisibilityPublic
			}
		}
	}
	return e
}

// Stat implements filesystem.Driver.
func (d *Driver) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	a, err := d.object(key).Attrs(ctx)
	if err != nil {
		return filesystem.Entry{}, mapErr(err)
	}
	return d.entry(a), nil
}

// Read implements filesystem.Driver. A range starting past the end is an
// empty read, after confirming the object exists. Objects are read as
// stored (see openRange).
func (d *Driver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	length := int64(-1)
	if rng.Length > 0 {
		length = rng.Length
	}
	r, err := d.openRange(ctx, key, rng.Offset, length)
	if statusOf(err) == http.StatusRequestedRangeNotSatisfiable {
		if _, serr := d.Stat(ctx, key); serr != nil {
			return nil, serr
		}
		return io.NopCloser(strings.NewReader("")), nil
	}
	if err != nil {
		return nil, mapErr(err)
	}
	return r, nil
}

// openRange opens a range of an object as stored. GCS decompresses an
// object stored with Content-Encoding gzip on the way out, which would
// change its bytes and size, so such a read is reopened with
// ReadCompressed. Only those objects pay the second request; and a client
// built with storage.WithJSONReads, which refuses compressed reads, fails
// on them with an error rather than returning other bytes.
func (d *Driver) openRange(ctx context.Context, key string, offset, length int64) (*storage.Reader, error) {
	obj := d.object(key)
	r, err := obj.NewRangeReader(ctx, offset, length)
	if err != nil || !r.Attrs.Decompressed {
		return r, err
	}
	_ = r.Close()
	return obj.ReadCompressed(true).NewRangeReader(ctx, offset, length)
}

// Write implements filesystem.Driver. A failing body cancels the upload, so
// GCS never commits a partial object.
func (d *Driver) Write(ctx context.Context, key string, body io.Reader, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	obj := d.object(key)
	switch {
	case opts.IfNotExists:
		obj = obj.If(storage.Conditions{DoesNotExist: true})
	case opts.IfMatch != "":
		cond, err := d.swapCondition(ctx, key, opts.IfMatch)
		if err != nil {
			return filesystem.Entry{}, err
		}
		obj = obj.If(cond)
	}
	call, err := d.callSettings(ctx, key)
	if err != nil {
		return filesystem.Entry{}, err
	}
	w := obj.NewWriter(ctx)
	w.StorageClass = d.storageClass(call)
	w.Retention = d.writeRetention(call)
	w.TemporaryHold = call.TemporaryHold
	w.CustomTime = call.CustomTime
	w.ContentType = opts.ContentType
	w.Metadata = opts.Metadata
	w.KMSKeyName = d.kmsKey(call, key)
	w.CacheControl = opts.Headers.CacheControl
	w.ContentDisposition = opts.Headers.ContentDisposition
	w.ContentEncoding = opts.Headers.ContentEncoding
	w.ChunkSize = d.chunkSize()
	if d.cfg.Visibility == VisibilityACL {
		w.PredefinedACL = string(d.privateACL())
		if opts.Visibility == filesystem.VisibilityPublic {
			w.PredefinedACL = aclPublicRead
		}
	}
	// Read up to one chunk first. A body that fits is uploaded only after it
	// was read completely, so a failing body never reaches GCS. A larger one
	// streams as a resumable upload, which GCS commits only when the final
	// chunk is sent; cancelling before that leaves nothing behind.
	head, err := io.ReadAll(io.LimitReader(body, int64(d.chunkSize())+1))
	if err != nil {
		cancel()
		_ = w.Close()
		return filesystem.Entry{}, err
	}
	rest := body
	if len(head) <= d.chunkSize() {
		rest = strings.NewReader("")
	}
	if _, err := io.Copy(w, io.MultiReader(bytes.NewReader(head), rest)); err != nil {
		cancel()
		_ = w.Close()
		return filesystem.Entry{}, err
	}
	if err := w.Close(); err != nil {
		if opts.IfMatch != "" && statusOf(err) == http.StatusPreconditionFailed {
			return filesystem.Entry{}, filesystem.NewError(filesystem.CodeConflict, err)
		}
		return filesystem.Entry{}, mapErr(err)
	}
	return d.entry(w.Attrs()), nil
}

// swapCondition turns a compare-and-swap ETag into the generation
// precondition GCS checks atomically (its JSON API has no If-Match on
// ETags): the object is read once, its ETag compared, and the write is
// then conditioned on that generation and metageneration. A writer that
// replaced the object in between changed the generation, so the write is
// refused (CodeConflict) rather than clobbering it.
func (d *Driver) swapCondition(ctx context.Context, key, etag string) (storage.Conditions, error) {
	a, err := d.object(key).Attrs(ctx)
	switch {
	case errors.Is(err, storage.ErrObjectNotExist):
		return storage.Conditions{}, filesystem.Errorf(filesystem.CodeConflict, "gcs: %q no longer exists", key)
	case err != nil:
		return storage.Conditions{}, mapErr(err)
	case strings.Trim(a.Etag, `"`) != etag:
		return storage.Conditions{}, filesystem.Errorf(filesystem.CodeConflict, "gcs: %q changed since ETag %s", key, etag)
	}
	return storage.Conditions{GenerationMatch: a.Generation, MetagenerationMatch: a.Metageneration}, nil
}

// Delete implements filesystem.Driver; deleting a missing object succeeds.
func (d *Driver) Delete(ctx context.Context, key string) error {
	err := d.bucket.Object(key).Delete(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil
	}
	return mapErr(err)
}

// List implements filesystem.Driver. The cursor is GCS's page token.
func (d *Driver) List(ctx context.Context, dir string, opts filesystem.ListOptions) (filesystem.Page, error) {
	q := &storage.Query{}
	if dir != "" {
		q.Prefix = dir + "/"
	}
	if !opts.Recursive {
		q.Delimiter = "/"
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = filesystem.DefaultPageSize
	}
	var attrs []*storage.ObjectAttrs
	next, err := iterator.NewPager(d.bucket.Objects(ctx, q), limit, opts.Cursor).NextPage(&attrs)
	if err != nil {
		return filesystem.Page{}, mapErr(err)
	}
	page := filesystem.Page{NextCursor: next}
	defer func() {
		// GCS returns objects before prefixes; the contract is key order.
		slices.SortFunc(page.Entries, func(a, b filesystem.Entry) int { return strings.Compare(a.Key, b.Key) })
	}()
	for _, a := range attrs {
		switch {
		case a.Prefix != "":
			page.Entries = append(page.Entries, filesystem.Entry{Key: strings.TrimSuffix(a.Prefix, "/"), IsDir: true})
		case strings.HasSuffix(a.Name, "/"):
			// A directory marker, not an object (keys never end in "/").
		default:
			page.Entries = append(page.Entries, d.entry(a))
		}
	}
	return page, nil
}

// Copy implements filesystem.Copier server-side. In ACL mode the source's
// visibility is carried over, because a copy gets the bucket's default ACL.
// The copy is a new object, so it gets Config.RetainFor like a Write, and
// the call's RequestOptions retention, hold and Custom-Time.
func (d *Driver) Copy(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	call, err := d.callSettings(ctx, dst)
	if err != nil {
		return filesystem.Entry{}, err
	}
	c := d.object(dst).CopierFrom(d.object(src))
	c.DestinationKMSKeyName = d.kmsKey(call, dst)
	// Only a class the call asks for: a copy otherwise keeps the bucket's
	// default class, as it always has.
	if call.StorageClass != "" {
		c.StorageClass = string(call.StorageClass)
	}
	c.Retention = d.writeRetention(call)
	c.TemporaryHold = call.TemporaryHold
	c.CustomTime = call.CustomTime
	// A configured PrivateACL is the copy's base ACL, so a private copy
	// carries the owner's grant; SetVisibility below then adds the public
	// grant for a public source. Without one the copy keeps the bucket's
	// default ACL, as it always has.
	if d.cfg.PrivateACL != "" {
		c.PredefinedACL = string(d.cfg.PrivateACL)
	}
	a, err := c.Run(ctx)
	if err != nil {
		return filesystem.Entry{}, mapErr(err)
	}
	if d.cfg.Visibility == VisibilityACL {
		srcAttrs, err := d.Stat(ctx, src)
		if err != nil {
			return filesystem.Entry{}, err
		}
		if err := d.SetVisibility(ctx, dst, srcAttrs.Visibility); err != nil {
			return filesystem.Entry{}, err
		}
		return d.Stat(ctx, dst)
	}
	return d.entry(a), nil
}

// SetVisibility implements filesystem.VisibilitySetter in ACL mode.
func (d *Driver) SetVisibility(ctx context.Context, key string, v filesystem.Visibility) error {
	acl := d.bucket.Object(key).ACL()
	if v == filesystem.VisibilityPublic {
		return mapErr(acl.Set(ctx, storage.AllUsers, storage.RoleReader))
	}
	if _, err := d.Stat(ctx, key); err != nil {
		return err
	}
	err := acl.Delete(ctx, storage.AllUsers)
	if statusOf(err) == http.StatusNotFound {
		return nil
	}
	return mapErr(err)
}

// headerKMSKeyName names the Cloud KMS key on an XML API upload; a
// presigned PUT signs it so a direct upload is encrypted like Write.
const headerKMSKeyName = "X-Goog-Encryption-Kms-Key-Name"

// XML API headers (and form fields, lower-cased) a presigned upload signs
// when the call asks for them.
const (
	// headerStorageClass is RequestOptions.StorageClass.
	headerStorageClass = "X-Goog-Storage-Class"
	// headerACL is a visibility asked for (PresignOptions.Visibility); the
	// form field is postFieldACL.
	headerACL = "X-Goog-Acl"
)

// xmlACLPublicRead is the XML API canned ACL a public visibility is signed
// as; it differs from the JSON API's aclPublicRead Write uses. A private
// one is signed as Driver.privateACL's xml name.
const xmlACLPublicRead = "public-read"

// presignACL is the canned ACL a presigned PUT or form pins for the
// visibility the Disk passed. For "" it pins Config.PrivateACL when one is
// set (so a cross-project upload still hands the bucket's owner its
// grant), and otherwise nothing, which leaves the bucket's default object
// ACL exactly as before visibility could be asked for. With
// uniform bucket-level access (VisibilityNone) GCS refuses ACLs, so a
// visibility is CodeUnsupported rather than an upload GCS would reject;
// the Disk never passes one then, because the driver declares no
// PresignVisibility.
func (d *Driver) presignACL(v filesystem.Visibility) (string, error) {
	switch {
	case v == "" && d.cfg.PrivateACL != "":
		return d.cfg.PrivateACL.xml(), nil
	case v == "":
		return "", nil
	case d.cfg.Visibility != VisibilityACL:
		return "", filesystem.Errorf(filesystem.CodeUnsupported, "gcs: a presigned upload can pin a visibility only in ACL mode; this bucket uses uniform access")
	case v == filesystem.VisibilityPublic:
		return xmlACLPublicRead, nil
	default:
		return d.privateACL().xml(), nil
	}
}

// Presign implements filesystem.Presigner with V4 signing. A PUT signs the
// content type and opts.Headers (Cache-Control, Content-Disposition,
// Content-Encoding), so the client must send exactly the headers in
// SignedRequest.Header; GCS refuses the upload otherwise and stores them
// with the object (PresignHeaders). It signs the disk's KMS key too, the
// call's RequestOptions (KMSKeyName, StorageClass) for key, and, in ACL
// mode, a visibility asked for as x-goog-acl (PresignVisibility), so the
// client cannot make the object public on its own.
func (d *Driver) Presign(ctx context.Context, key string, opts filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	so := *d.cfg.Signing
	if d.cfg.IAM != nil && so.PrivateKey == nil && so.SignBytes == nil {
		so.SignBytes = d.signBlob(ctx, so.GoogleAccessID)
	}
	so.Scheme = storage.SigningSchemeV4
	so.Method = string(opts.Method)
	expires := time.Now().Add(opts.Expires)
	so.Expires = expires
	var header http.Header
	switch opts.Method {
	case filesystem.MethodPut:
		call, err := d.presignSettings(ctx, key)
		if err != nil {
			return filesystem.SignedRequest{}, err
		}
		acl, err := d.presignACL(opts.Visibility)
		if err != nil {
			return filesystem.SignedRequest{}, err
		}
		so.ContentType = opts.ContentType
		header = http.Header{}
		if opts.ContentType != "" {
			header.Set("Content-Type", opts.ContentType)
		}
		opts.Headers.SetOn(header)
		if kms := d.kmsKey(call, key); kms != "" {
			header.Set(headerKMSKeyName, kms)
		}
		if call.StorageClass != "" {
			header.Set(headerStorageClass, string(call.StorageClass))
		}
		if acl != "" {
			header.Set(headerACL, acl)
		}
		// The SDK signs each "Name:value" entry and lists it in
		// X-Goog-SignedHeaders.
		for _, name := range []string{filesystem.HeaderCacheControl, filesystem.HeaderContentDisposition, filesystem.HeaderContentEncoding, headerKMSKeyName, headerStorageClass, headerACL} {
			if v := header.Get(name); v != "" {
				so.Headers = append(so.Headers, name+":"+v)
			}
		}
		if len(header) == 0 {
			header = nil
		}
	default:
		q := url.Values{}
		if opts.ContentType != "" {
			q.Set("response-content-type", opts.ContentType)
		}
		if opts.ContentDisposition != "" {
			q.Set("response-content-disposition", opts.ContentDisposition)
		}
		so.QueryParameters = q
	}
	u, err := d.bucket.SignedURL(key, &so)
	if err != nil {
		return filesystem.SignedRequest{}, mapErr(err)
	}
	return filesystem.SignedRequest{Method: opts.Method, URL: u, Header: header, ExpiresAt: expires}, nil
}

// signBlob returns a SignBytes that has Google sign as account through
// Config.IAM, bounded by ctx.
func (d *Driver) signBlob(ctx context.Context, account string) func([]byte) ([]byte, error) {
	return func(payload []byte) ([]byte, error) {
		req := &iamcredentials.SignBlobRequest{Payload: base64.StdEncoding.EncodeToString(payload)}
		resp, err := d.cfg.IAM.Projects.ServiceAccounts.SignBlob("projects/-/serviceAccounts/"+account, req).Context(ctx).Do()
		if err != nil {
			return nil, err
		}
		return base64.StdEncoding.DecodeString(resp.SignedBlob)
	}
}
