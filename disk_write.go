package filesystem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"

	"github.com/ubgo/filesystem/sniff"
)

// Write stores body under key, replacing any existing object, and returns
// the stored entry with Size and SHA256 computed by the Disk while
// streaming.
//
// The Disk validates opts against the driver's capabilities first, so
// nothing is silently dropped: Metadata needs Capabilities.Metadata, Headers
// need Capabilities.Headers,
// IfNotExists needs Capabilities.ConditionalWrite, and a Visibility other
// than the disk default needs Capabilities.SetVisibility. When ContentType
// is empty it is detected from the first bytes and the key's extension.
//
// A write is retried by a retry interceptor only when body implements
// io.Seeker and can actually seek (a pipe's *os.File cannot, so it is
// written once as a stream), because a consumed stream cannot be replayed.
func (d *Disk) Write(ctx context.Context, key string, body io.Reader, opts WriteOptions) (Entry, error) {
	k, err := d.CleanKey(key)
	if err != nil {
		return Entry{}, annotate(err, OpWrite, d.name, key)
	}
	if body == nil {
		return Entry{}, annotate(Errorf(CodeInvalidArgument, "nil body"), OpWrite, d.name, k)
	}
	if err := d.prepareWrite(&opts); err != nil {
		return Entry{}, annotate(err, OpWrite, d.name, k)
	}
	src, err := newWriteSource(body, opts.MaxSize, opts.SizeHint, opts.SHA256, opts.Checksums)
	if err != nil {
		return Entry{}, annotate(err, OpWrite, d.name, k)
	}
	if opts.ContentType == "" && d.detectContentType {
		opts.ContentType = sniff.DetectWith(d.detector, src.head, k)
	}
	var stored Entry
	op := &Op{Kind: OpWrite, Key: k, Mutating: true, Replayable: src.replayable()}
	err = d.do(ctx, op, func(ctx context.Context) error {
		r, err := src.open()
		if err != nil {
			return err
		}
		e, err := d.driver.Write(ctx, k, op.wrapBody(r), opts)
		if r.tooLarge {
			if err == nil {
				// The driver ignored the reader's error and committed a
				// truncated object. The contract forbids it; remove the
				// object so the "nothing larger is stored" promise holds.
				_ = d.driver.Delete(ctx, k)
			}
			return Errorf(CodeTooLarge, "body exceeds %d bytes", opts.MaxSize)
		}
		if r.checksumMismatch != "" {
			if err == nil {
				_ = d.driver.Delete(ctx, k)
			}
			return Errorf(CodeInvalidArgument, "body does not match the declared %s", r.checksumMismatch)
		}
		if r.mismatch {
			if err == nil {
				// As above: a driver that committed despite the reader's
				// error must not leave a truncated or overlong object.
				_ = d.driver.Delete(ctx, k)
			}
			return Errorf(CodeInvalidArgument, "body is not the %d bytes SizeHint declared", opts.SizeHint)
		}
		if err != nil {
			return err
		}
		e.Key = k
		e.Size = r.n
		e.SHA256 = hex.EncodeToString(r.hash.Sum(nil))
		e.Checksums = r.sums()
		// A driver that does not store content types derives one (from the
		// extension at best); the type the Disk detected or was given is
		// more accurate for the entry it returns.
		if opts.ContentType != "" && (!d.caps.ContentType || e.ContentType == "") {
			e.ContentType = opts.ContentType
		}
		if e.Visibility == "" && d.caps.SetVisibility {
			e.Visibility = opts.Visibility
		}
		stored = e
		op.Bytes = r.n
		return nil
	})
	if err != nil {
		return Entry{}, err
	}
	return stored, nil
}

// WriteBytes stores b under key. It is Write with a rewindable body, so it
// is always retryable.
func (d *Disk) WriteBytes(ctx context.Context, key string, b []byte, opts WriteOptions) (Entry, error) {
	if opts.SizeHint == 0 {
		opts.SizeHint = int64(len(b))
	}
	return d.Write(ctx, key, bytes.NewReader(b), opts)
}

// prepareWrite fills defaults into opts and rejects options the driver
// cannot honour.
func (d *Disk) prepareWrite(opts *WriteOptions) error {
	if opts.MaxSize < 0 || opts.SizeHint < 0 {
		return Errorf(CodeInvalidArgument, "negative MaxSize or SizeHint")
	}
	if opts.MaxSize > 0 && opts.SizeHint > opts.MaxSize {
		return Errorf(CodeTooLarge, "declared size %d exceeds %d bytes", opts.SizeHint, opts.MaxSize)
	}
	switch {
	case opts.Visibility == "":
		opts.Visibility = d.defaultVisibility
	case !opts.Visibility.Valid():
		return Errorf(CodeInvalidArgument, "unknown visibility %q", opts.Visibility)
	case opts.Visibility != d.defaultVisibility && !d.caps.SetVisibility:
		return Errorf(CodeUnsupported, "driver %T stores no per-object visibility; disk default is %s", d.driver, d.defaultVisibility)
	}
	if len(opts.Metadata) > 0 && !d.caps.Metadata {
		return Errorf(CodeUnsupported, "driver %T does not store metadata", d.driver)
	}
	if err := opts.Headers.validate(); err != nil {
		return err
	}
	if opts.SHA256 != "" {
		sum, err := hex.DecodeString(opts.SHA256)
		if err != nil || len(sum) != sha256.Size {
			return Errorf(CodeInvalidArgument, "SHA256 is not %d hex characters", 2*sha256.Size)
		}
		opts.SHA256 = hex.EncodeToString(sum) // lowercase, as backends and Entry.SHA256 write it
	}
	checksums, err := prepareChecksums(opts.Checksums)
	if err != nil {
		return err
	}
	opts.Checksums = checksums
	if !opts.Headers.IsZero() && !d.caps.Headers {
		return Errorf(CodeUnsupported, "driver %T does not store HTTP headers", d.driver)
	}
	if opts.IfMatch != "" && opts.IfNotExists {
		return Errorf(CodeInvalidArgument, "IfMatch and IfNotExists cannot both be set")
	}
	if opts.IfMatch != "" && !d.caps.CompareAndSwap {
		return Errorf(CodeUnsupported, "driver %T has no compare-and-swap write", d.driver)
	}
	// Drivers compare against the bare ETag they report, so quotes or a
	// weak prefix copied from an HTTP header do not cause a conflict.
	opts.IfMatch = trimETag(opts.IfMatch)
	if opts.IfNotExists && !d.caps.ConditionalWrite {
		return Errorf(CodeUnsupported, "driver %T has no atomic conditional write", d.driver)
	}
	return nil
}

// writeSource turns the caller's body into a reader that the Disk can count,
// hash, cap at MaxSize and, when the body is seekable, rewind for a retry.
type writeSource struct {
	body     io.Reader
	seeker   io.Seeker
	start    int64
	head     []byte
	maxSize  int64
	hint     int64
	sha256   string
	sums     []Checksum
	attempts int
}

// newWriteSource reads the first sniff.HeadSize bytes for content-type
// detection. A seekable body is rewound to where it started; a stream keeps
// those bytes and replays them before the rest.
//
// Implementing io.Seeker does not make a body seekable: an *os.File on a
// pipe, socket or terminal has Seek but it fails (ESPIPE). The first Seek
// happens before anything is read, so when it fails the body is treated as
// the one-shot stream it is (written once, never retried) instead of
// failing the write.
func newWriteSource(body io.Reader, maxSize, hint int64, digest string, sums []Checksum) (*writeSource, error) {
	s := &writeSource{body: body, maxSize: maxSize, hint: hint, sha256: digest, sums: sums}
	if sk, ok := body.(io.Seeker); ok {
		if start, err := sk.Seek(0, io.SeekCurrent); err == nil {
			s.seeker, s.start = sk, start
		}
	}
	head := make([]byte, sniff.HeadSize)
	n, err := io.ReadFull(body, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	s.head = head[:n]
	if s.seeker != nil {
		if _, err := s.seeker.Seek(s.start, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *writeSource) replayable() bool { return s.seeker != nil }

// open returns the reader for one attempt.
func (s *writeSource) open() (*meteredReader, error) {
	s.attempts++
	var r io.Reader
	switch {
	case s.seeker != nil:
		if s.attempts > 1 {
			if _, err := s.seeker.Seek(s.start, io.SeekStart); err != nil {
				return nil, err
			}
		}
		r = s.body
	case s.attempts == 1:
		r = io.MultiReader(bytes.NewReader(s.head), s.body)
	default:
		return nil, Errorf(CodeInvalidArgument, "write body is a stream and cannot be replayed")
	}
	return &meteredReader{r: r, max: s.maxSize, hint: s.hint, want: s.sha256, hash: sha256.New(), extra: startChecksums(s.sums)}, nil
}

// meteredReader counts and hashes what passes through it, fails once the
// byte count passes max (when max > 0), and fails when the body is longer
// or shorter than hint (when hint > 0), so a truncated or padded body is
// never stored as if it were whole.
type meteredReader struct {
	r    io.Reader
	max  int64
	hint int64
	want string // expected hex SHA-256, "" for none
	n    int64
	hash hash.Hash
	// extra are the WriteOptions.Checksums of this attempt.
	extra    []runningChecksum
	tooLarge bool
	mismatch bool
	// checksumMismatch names the digest the body ended without ("SHA-256",
	// or a Checksum.Name); empty while every declared digest holds.
	checksumMismatch string
}

// checksumSHA256 names the built-in digest in a mismatch error.
const checksumSHA256 = "SHA-256"

// sums returns the extra digests in lowercase hex by name, nil for none.
func (m *meteredReader) sums() map[string]string {
	if len(m.extra) == 0 {
		return nil
	}
	out := make(map[string]string, len(m.extra))
	for _, c := range m.extra {
		out[c.name] = hex.EncodeToString(c.hash.Sum(nil))
	}
	return out
}

// errChecksum is what the driver sees instead of the end of the body when
// the body's SHA-256 differs from WriteOptions.SHA256 (or a digest from
// WriteOptions.Checksums from its Want), so it commits
// nothing. The Disk replaces the driver's resulting error.
var errChecksum = Errorf(CodeInvalidArgument, "body does not match the declared SHA-256")

// verify names the first declared digest the whole body does not have,
// or returns "" when every one holds.
func (m *meteredReader) verify() string {
	if m.want != "" && hex.EncodeToString(m.hash.Sum(nil)) != m.want {
		return checksumSHA256
	}
	for _, c := range m.extra {
		if c.want != "" && hex.EncodeToString(c.hash.Sum(nil)) != c.want {
			return c.name
		}
	}
	return ""
}

// errSizeMismatch is what the driver sees from the body when its length
// differs from WriteOptions.SizeHint. The Disk replaces the driver's
// resulting error with a CodeInvalidArgument one.
var errSizeMismatch = Errorf(CodeInvalidArgument, "body length differs from SizeHint")

// errTooLarge is what the driver sees from the body when the cap is passed.
// The Disk replaces the driver's resulting error with a CodeTooLarge one.
var errTooLarge = Errorf(CodeTooLarge, "body exceeds the size limit")

func (m *meteredReader) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	m.n += int64(n)
	m.hash.Write(p[:n])
	for _, c := range m.extra {
		c.hash.Write(p[:n])
	}
	if m.max > 0 && m.n > m.max {
		m.tooLarge = true
		return n, errTooLarge
	}
	if m.hint > 0 && (m.n > m.hint || (errors.Is(err, io.EOF) && m.n < m.hint)) {
		m.mismatch = true
		return n, errSizeMismatch
	}
	if errors.Is(err, io.EOF) {
		if name := m.verify(); name != "" {
			m.checksumMismatch = name
			return n, errChecksum
		}
	}
	return n, err
}

// Copy copies src to dst on this disk, replacing dst. With the Copy
// capability the backend copies server-side; otherwise the object streams
// through the application as a Read followed by a Write, keeping its content
// type, metadata and visibility where the driver stores them.
func (d *Disk) Copy(ctx context.Context, src, dst string) (Entry, error) {
	s, t, err := d.cleanPair(OpCopy, src, dst)
	if err != nil {
		return Entry{}, err
	}
	if !d.caps.Copy {
		return d.streamCopy(ctx, s, t)
	}
	copier := d.driver.(Copier)
	var e Entry
	op := &Op{Kind: OpCopy, Key: s, Target: t, Mutating: true, Replayable: true}
	err = d.do(ctx, op, func(ctx context.Context) error {
		var err error
		e, err = copier.Copy(ctx, s, t)
		return err
	})
	if err != nil {
		return Entry{}, err
	}
	e.Key = t
	return e, nil
}

// Move renames src to dst on this disk, replacing dst. With the Move
// capability it is a backend rename; otherwise it is Copy then Delete of
// src, which is not atomic: a failure between the two leaves both keys.
func (d *Disk) Move(ctx context.Context, src, dst string) (Entry, error) {
	s, t, err := d.cleanPair(OpMove, src, dst)
	if err != nil {
		return Entry{}, err
	}
	if !d.caps.Move {
		e, err := d.Copy(ctx, s, t)
		if err != nil {
			return Entry{}, err
		}
		if err := d.Delete(ctx, s); err != nil {
			return Entry{}, err
		}
		return e, nil
	}
	mover := d.driver.(Mover)
	var e Entry
	op := &Op{Kind: OpMove, Key: s, Target: t, Mutating: true, Replayable: false}
	err = d.do(ctx, op, func(ctx context.Context) error {
		var err error
		e, err = mover.Move(ctx, s, t)
		return err
	})
	if err != nil {
		return Entry{}, err
	}
	e.Key = t
	return e, nil
}

// cleanPair cleans a source and destination key and refuses copying a key
// onto itself, which no backend handles the same way.
func (d *Disk) cleanPair(kind OpKind, src, dst string) (string, string, error) {
	s, err := d.CleanKey(src)
	if err != nil {
		return "", "", annotate(err, kind, d.name, src)
	}
	t, err := d.CleanKey(dst)
	if err != nil {
		return "", "", annotate(err, kind, d.name, dst)
	}
	if s == t {
		return "", "", annotate(Errorf(CodeInvalidArgument, "source and destination are the same key"), kind, d.name, s)
	}
	return s, t, nil
}

// streamCopy is Copy for drivers without server-side copy.
func (d *Disk) streamCopy(ctx context.Context, src, dst string) (Entry, error) {
	info, err := d.Stat(ctx, src)
	if err != nil {
		return Entry{}, err
	}
	rc, err := d.Read(ctx, src)
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = rc.Close() }()
	opts := WriteOptions{ContentType: info.ContentType, SizeHint: info.Size}
	if d.caps.Metadata {
		opts.Metadata = info.Metadata
	}
	if d.caps.Headers {
		opts.Headers = info.Headers
	}
	if d.caps.SetVisibility && info.Visibility.Valid() {
		opts.Visibility = info.Visibility
	}
	return d.Write(ctx, dst, rc, opts)
}
