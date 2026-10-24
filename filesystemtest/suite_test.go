package filesystemtest_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/filesystemtest"
	"github.com/ubgo/filesystem/memory"
)

func TestSuitePassesOnTheReferenceDriver(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		d, err := filesystem.NewDisk("mem", memory.New())
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
}

// bare is the memory driver declaring no optional capability, so the cases
// for undeclared capabilities (the "UnsupportedIsRefused" ones) run and the
// Disk's portable fallbacks are exercised.
type bare struct{ *memory.Driver }

func (bare) Capabilities() filesystem.Capabilities { return filesystem.Capabilities{} }

func TestSuitePassesWithoutOptionalCapabilities(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		d, err := filesystem.NewDisk("bare", bare{memory.New()})
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
}

// fault names one way a driver can break its contract.
type fault string

const (
	corruptRead        fault = "corrupt-read"
	noOverwrite        fault = "no-overwrite"
	wrongSize          fault = "wrong-size"
	truncateLarge      fault = "truncate-large"
	plainStatError     fault = "plain-stat-error"
	plainReadError     fault = "plain-read-error"
	zeroModTime        fault = "zero-mod-time"
	phantomStat        fault = "phantom-stat"
	deleteNoop         fault = "delete-noop"
	deleteMissing      fault = "delete-missing-fails"
	ignoreRange        fault = "ignore-range"
	asciiKeys          fault = "ascii-keys"
	partialOnError     fault = "partial-on-error"
	wrongType          fault = "wrong-type"
	dropType           fault = "drop-type"
	dropMetadata       fault = "drop-metadata"
	dropHeaders        fault = "drop-headers"
	copyDropsHeaders   fault = "copy-drops-headers"
	ignoreIfMissing    fault = "ignore-if-not-exists"
	visibilityNoop     fault = "visibility-noop"
	unsorted           fault = "unsorted"
	noFolding          fault = "no-folding"
	ignoreLimit        fault = "ignore-limit"
	missingDirError    fault = "missing-dir-error"
	rawPrefix          fault = "raw-prefix"
	copyNoop           fault = "copy-noop"
	copyPhantom        fault = "copy-phantom"
	moveKeeps          fault = "move-keeps-source"
	deleteRawPrefix    fault = "delete-raw-prefix"
	deleteFirstKeyOnly fault = "delete-first-key-only"
	lossyWrites        fault = "lossy-writes"
	reversedParts      fault = "reversed-parts"
	abortNoop          fault = "abort-noop"
	rangeReadError     fault = "range-read-error"
	partialOverwrite   fault = "partial-overwrite"
	writeVisibility    fault = "ignore-write-visibility"
	ignoreCursor       fault = "ignore-cursor"
	dropListedPart     fault = "drop-listed-part"
	sweepNoop          fault = "sweep-noop"
	sweepEverything    fault = "sweep-ignores-cutoff"
	sweepObjects       fault = "sweep-deletes-objects"
	// The faults of the case for drivers without Multipart hide it.
	sweepAllNoMultipart    fault = "sweep-deletes-all-without-multipart"
	sweepBreaksNoMultipart fault = "sweep-fails-without-multipart"
	postBadURL             fault = "post-relative-url"
	postWrongKey           fault = "post-wrong-key"
	postNoContentType      fault = "post-no-content-type"
	postNoHeaders          fault = "post-drops-headers"
	postLongExpiry         fault = "post-long-expiry"
	postIgnoresPrefix      fault = "post-ignores-prefix"
	postAlwaysTyped        fault = "post-always-typed"
	postFails              fault = "post-fails"
	postPrefixFails        fault = "post-prefix-fails"
	appendReplaces         fault = "append-replaces"
	appendKeepsPartial     fault = "append-keeps-partial"
	appendUnlocked         fault = "append-unlocked"
	casIgnoresMatch        fault = "cas-ignores-if-match"
	casWrongCode           fault = "cas-wrong-code"
	modTimeNoop            fault = "mod-time-noop"
	tagsNoop               fault = "tags-noop"
	unstableETag           fault = "unstable-etag"
	caseFolding            fault = "undeclared-case-folding"
	charsMangled           fault = "chars-mangled"
	appendTruncates        fault = "append-truncates"
	appendIgnoresMax       fault = "append-ignores-max-size"
	casUnserialized        fault = "cas-unserialized"
	listMangles            fault = "list-mangles-keys"
	classNoop              fault = "storage-class-noop"
	versionReadsLatest     fault = "version-reads-latest"
	restoreVersionNoop     fault = "restore-version-noop"
)

// breakers maps every case to the fault it must detect. Cases that check a
// guarantee the Disk enforces itself (so no driver can break it) are listed
// in diskGuaranteed instead.
var breakers = map[string][]fault{
	"WriteThenRead":                 {corruptRead},
	"OverwriteReplaces":             {noOverwrite},
	"EmptyObject":                   {wrongSize},
	"LargeObject":                   {truncateLarge},
	"StatMissingIsNotFound":         {plainStatError},
	"ReadMissingIsNotFound":         {plainReadError},
	"StatReportsSizeAndTime":        {zeroModTime},
	"Exists":                        {phantomStat},
	"DeleteRemoves":                 {deleteNoop},
	"DeleteMissingSucceeds":         {deleteMissing},
	"ReadRanges":                    {ignoreRange, rangeReadError},
	"UnicodeAndSpacesInKeys":        {asciiKeys},
	"FailingBodyStoresNothing":      {partialOnError, partialOverwrite},
	"DetectsContentType":            {wrongType},
	"StoresContentType":             {dropType},
	"StoresMetadata":                {dropMetadata},
	"StoresHeaders":                 {dropHeaders, copyDropsHeaders},
	"ConditionalWrite":              {ignoreIfMissing},
	"Visibility":                    {visibilityNoop, writeVisibility},
	"ListRecursive":                 {unsorted},
	"ListFoldsDirectories":          {noFolding},
	"ListPaginates":                 {ignoreLimit, ignoreCursor},
	"ListEmptyDirectory":            {missingDirError},
	"ListDoesNotMatchSiblingPrefix": {rawPrefix},
	"CopyDuplicates":                {copyNoop},
	"CopyMissingIsNotFound":         {copyPhantom},
	"MoveRenames":                   {moveKeeps},
	"DeletePrefixScoped":            {deleteRawPrefix},
	"DeleteKeys":                    {deleteFirstKeyOnly},
	"ConcurrentWrites":              {lossyWrites},
	"Multipart":                     {reversedParts, dropListedPart},
	"MultipartAbort":                {abortNoop},
	"SweepAbandoned":                {sweepNoop, sweepEverything, sweepObjects},
	"SweepAbandonedKeepsObjects":    {sweepAllNoMultipart, sweepBreaksNoMultipart},
	"Append":                        {appendReplaces, appendKeepsPartial, appendUnlocked, appendTruncates, appendIgnoresMax},
	"CompareAndSwap":                {casIgnoresMatch, casWrongCode, casUnserialized},
	"SetModTime":                    {modTimeNoop},
	"Tags":                          {tagsNoop},
	"StorageClass":                  {classNoop},
	"VersionRead":                   {versionReadsLatest, restoreVersionNoop},
	"ConditionalRead":               {unstableETag, corruptRead},
	"LetterCase":                    {caseFolding},
	"KeyCharactersRoundTrip":        {charsMangled, asciiKeys, listMangles},
	"DirExists":                     {dirIgnoresKeys, dirObjectIsDir},
	"MakeDir":                       {makeDirNoop, makeDirOverObject, dirSurvivesDeletePrefix, makeDirMarker},
	"FindAbandoned":                 {findNoop, findIgnoresCutoff, findRemoves, findWrongKey},
	"FindAbandonedKeepsObjects":     {findReportsObjects, findDeletesNoMultipart, findFailsNoMultipart},
	"PresignPostForm":               {postBadURL, postWrongKey, postNoContentType, postNoHeaders, postLongExpiry, postIgnoresPrefix, postAlwaysTyped, postFails, postPrefixFails},
}

// diskGuaranteed lists cases whose property the Disk computes or enforces
// regardless of the driver, with the reason no driver fault can break them.
var diskGuaranteed = map[string]string{
	"WriteReportsSizeAndHash":              "the Disk counts and hashes the bytes it streams",
	"MaxSizeStoresNothing":                 "the Disk caps the body and deletes anything a driver commits anyway",
	"MetadataUnsupportedIsRefused":         "the Disk refuses metadata before calling a driver without the capability",
	"HeadersUnsupportedIsRefused":          "the Disk refuses headers before calling a driver without the capability",
	"ConditionalWriteUnsupportedIsRefused": "the Disk refuses IfNotExists before calling a driver without the capability",
	"SweepAbandonedUnsupportedIsRefused":   "the Disk refuses the sweep before calling a driver without the capability",
	"PresignPostUnsupportedIsRefused":      "the Disk refuses a POST form before calling a driver without the capability",
	"AppendUnsupportedIsRefused":           "the Disk refuses an append before calling a driver without the capability",
	"CompareAndSwapUnsupportedIsRefused":   "the Disk refuses IfMatch before calling a driver without the capability",
	"StorageClassUnsupportedIsRefused":     "the Disk refuses a class change before calling a driver without the capability",
	"MakeDirWithoutDirsWritesNothing":      "without the Dirs capability the Disk's MakeDir calls no driver method",
	"FindAbandonedUnsupportedIsRefused":    "the Disk refuses the preview before calling a driver without the capability",
}

func TestEveryCaseIsProvenByBreaking(t *testing.T) {
	for _, c := range filesystemtest.Cases() {
		_, broken := breakers[c.Name]
		_, guaranteed := diskGuaranteed[c.Name]
		if broken == guaranteed {
			t.Errorf("case %s must appear in exactly one of breakers and diskGuaranteed", c.Name)
		}
	}
	for _, c := range filesystemtest.Cases() {
		for _, f := range breakers[c.Name] {
			t.Run(c.Name+"/"+string(f), func(t *testing.T) {
				d, err := filesystem.NewDisk("broken", &faulty{Driver: memory.New(memory.WithVersions()), fault: f})
				if err != nil {
					t.Fatal(err)
				}
				if c.Needs != nil && !c.Needs(d.Capabilities()) {
					t.Fatalf("faulty driver lacks the capability case %s needs", c.Name)
				}
				if err := c.Check(t.Context(), d); err == nil {
					t.Errorf("case %s did not detect fault %s", c.Name, f)
				}
			})
		}
	}
}

// faulty is the memory driver with exactly one contract violation.
type faulty struct {
	*memory.Driver
	fault  fault
	writes atomic.Int64
}

func (f *faulty) is(x fault) bool { return f.fault == x }

func (f *faulty) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	e, err := f.Driver.Stat(ctx, key)
	switch {
	case err != nil && f.is(plainStatError):
		return e, errors.New("no such thing")
	case err != nil && f.is(phantomStat):
		return filesystem.Entry{Key: key}, nil
	case err == nil && f.is(wrongSize):
		e.Size++
	case err == nil && f.is(zeroModTime):
		e.ModTime = time.Time{}
	case err == nil && f.is(dropType):
		e.ContentType = ""
	case err == nil && f.is(dropMetadata):
		e.Metadata = nil
	case err == nil && f.is(dropHeaders):
		e.Headers = filesystem.HTTPHeaders{}
	case err == nil && f.is(unstableETag):
		e.ETag = strconv.FormatInt(f.writes.Add(1), 10)
	}
	return e, err
}

func (f *faulty) Append(ctx context.Context, key string, body io.Reader, o filesystem.AppendOptions) (filesystem.Entry, error) {
	switch {
	case f.is(appendReplaces):
		return f.Driver.Write(ctx, key, body, filesystem.WriteOptions{Visibility: o.Visibility})
	case f.is(appendIgnoresMax):
		o.MaxSize = 0
	case f.is(appendTruncates):
		data, err := io.ReadAll(body)
		if err != nil {
			return filesystem.Entry{}, err
		}
		return f.Driver.Append(ctx, key, bytes.NewReader(data[:min(len(data), chunkMax)]), o)
	case f.is(appendKeepsPartial):
		data, _ := io.ReadAll(body)
		return f.Driver.Append(ctx, key, bytes.NewReader(data), o)
	case f.is(appendUnlocked):
		// Append in two halves, letting other appends in between.
		data, err := io.ReadAll(body)
		if err != nil {
			return filesystem.Entry{}, err
		}
		half := len(data) / 2
		if _, err := f.Driver.Append(ctx, key, bytes.NewReader(data[:half]), o); err != nil {
			return filesystem.Entry{}, err
		}
		time.Sleep(time.Millisecond)
		return f.Driver.Append(ctx, key, bytes.NewReader(data[half:]), o)
	}
	return f.Driver.Append(ctx, key, body, o)
}

func (f *faulty) SetModTime(ctx context.Context, key string, t time.Time) error {
	if f.is(modTimeNoop) {
		_, err := f.Driver.Stat(ctx, key)
		return err
	}
	return f.Driver.SetModTime(ctx, key, t)
}

func (f *faulty) SetTags(ctx context.Context, key string, tags map[string]string) error {
	if f.is(tagsNoop) {
		_, err := f.Driver.Stat(ctx, key)
		return err
	}
	return f.Driver.SetTags(ctx, key, tags)
}

func (f *faulty) SetStorageClass(ctx context.Context, key, class string) error {
	if f.is(classNoop) {
		_, err := f.Driver.Stat(ctx, key)
		return err
	}
	return f.Driver.SetStorageClass(ctx, key, class)
}

func (f *faulty) ReadVersion(ctx context.Context, key, id string) (io.ReadCloser, error) {
	if f.is(versionReadsLatest) {
		return f.Driver.Read(ctx, key, filesystem.ByteRange{})
	}
	return f.Driver.ReadVersion(ctx, key, id)
}

func (f *faulty) RestoreVersion(ctx context.Context, key, id string) error {
	if f.is(restoreVersionNoop) {
		return nil
	}
	return f.Driver.RestoreVersion(ctx, key, id)
}

func (f *faulty) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	if f.is(ignoreRange) {
		rng = filesystem.ByteRange{}
	}
	rc, err := f.Driver.Read(ctx, key, rng)
	if err != nil && f.is(plainReadError) {
		return nil, errors.New("cannot read")
	}
	if err == nil && f.is(rangeReadError) && rng.Offset > 0 {
		_ = rc.Close()
		return io.NopCloser(iotestErrReader{}), nil
	}
	if err == nil && f.is(corruptRead) {
		_ = rc.Close()
		return io.NopCloser(strings.NewReader("garbage")), nil
	}
	return rc, err
}

func (f *faulty) Write(ctx context.Context, key string, body io.Reader, o filesystem.WriteOptions) (filesystem.Entry, error) {
	switch {
	case f.is(noOverwrite):
		if _, err := f.Driver.Stat(ctx, key); err == nil {
			_, _ = io.Copy(io.Discard, body)
			return filesystem.Entry{Key: key}, nil
		}
	case f.is(truncateLarge):
		body = io.LimitReader(body, 1<<20)
	case f.is(casIgnoresMatch):
		o.IfMatch = ""
	case f.is(casUnserialized) && o.IfMatch != "":
		// Checks the ETag, then writes unconditionally after a pause: two
		// racing writers both pass the check.
		cur, err := f.Driver.Stat(ctx, key)
		if err != nil || cur.ETag != o.IfMatch {
			return filesystem.Entry{}, filesystem.ErrConflict
		}
		time.Sleep(10 * time.Millisecond)
		o.IfMatch = ""
	case f.is(casWrongCode) && o.IfMatch != "":
		e, err := f.Driver.Write(ctx, key, body, o)
		if filesystem.CodeOf(err) == filesystem.CodeConflict {
			return e, filesystem.ErrExists
		}
		return e, err
	case f.is(caseFolding):
		key = strings.ToLower(key)
	case f.is(charsMangled):
		key = strings.ReplaceAll(key, ":", "_")
	case f.is(asciiKeys):
		key = strings.Map(func(r rune) rune {
			if r > 127 || r == ' ' {
				return -1
			}
			return r
		}, key)
	case f.is(partialOnError):
		data, _ := io.ReadAll(body)
		return f.Driver.Write(ctx, key, bytes.NewReader(data), o)
	case f.is(partialOverwrite):
		if _, err := f.Driver.Stat(ctx, key); err == nil {
			data, _ := io.ReadAll(body)
			return f.Driver.Write(ctx, key, bytes.NewReader(data), o)
		}
	case f.is(writeVisibility):
		o.Visibility = filesystem.VisibilityPrivate
	case f.is(wrongType):
		o.ContentType = "application/x-wrong"
	case f.is(ignoreIfMissing):
		o.IfNotExists = false
	case f.is(lossyWrites):
		if f.writes.Add(1)%5 == 0 {
			_, _ = io.Copy(io.Discard, body)
			return filesystem.Entry{Key: key}, nil
		}
	}
	return f.Driver.Write(ctx, key, body, o)
}

func (f *faulty) Delete(ctx context.Context, key string) error {
	switch {
	case f.is(deleteNoop):
		return nil
	case f.is(deleteMissing):
		if _, err := f.Driver.Stat(ctx, key); err != nil {
			return err
		}
	}
	return f.Driver.Delete(ctx, key)
}

func (f *faulty) List(ctx context.Context, dir string, o filesystem.ListOptions) (filesystem.Page, error) {
	switch {
	case f.is(noFolding):
		o.Recursive = true
	case f.is(ignoreLimit):
		o.Limit, o.Cursor = 1<<30, ""
	case f.is(ignoreCursor):
		o.Cursor = ""
	case f.is(missingDirError) && dir != "":
		return filesystem.Page{}, filesystem.ErrNotFound
	case f.is(rawPrefix):
		all, err := f.Driver.List(ctx, "", filesystem.ListOptions{Recursive: true, Limit: 1 << 30})
		var out []filesystem.Entry
		for _, e := range all.Entries {
			if strings.HasPrefix(e.Key, dir) {
				out = append(out, e)
			}
		}
		return filesystem.Page{Entries: out}, err
	}
	p, err := f.Driver.List(ctx, dir, o)
	if f.is(unsorted) {
		slices.Reverse(p.Entries)
	}
	if f.is(listMangles) {
		for i := range p.Entries {
			p.Entries[i].Key = strings.ReplaceAll(p.Entries[i].Key, ":", "%3A")
		}
	}
	return p, err
}

func (f *faulty) Copy(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	switch {
	case f.is(copyNoop):
		return filesystem.Entry{Key: dst}, nil
	case f.is(copyPhantom):
		if _, err := f.Driver.Stat(ctx, src); err != nil {
			return filesystem.Entry{Key: dst}, nil
		}
	case f.is(copyDropsHeaders):
		rc, err := f.Driver.Read(ctx, src, filesystem.ByteRange{})
		if err != nil {
			return filesystem.Entry{}, err
		}
		defer func() { _ = rc.Close() }()
		return f.Driver.Write(ctx, dst, rc, filesystem.WriteOptions{})
	}
	return f.Driver.Copy(ctx, src, dst)
}

func (f *faulty) Move(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	if f.is(moveKeeps) {
		return f.Driver.Copy(ctx, src, dst)
	}
	return f.Driver.Move(ctx, src, dst)
}

func (f *faulty) SetVisibility(ctx context.Context, key string, v filesystem.Visibility) error {
	if f.is(visibilityNoop) {
		_, err := f.Driver.Stat(ctx, key)
		return err
	}
	return f.Driver.SetVisibility(ctx, key, v)
}

func (f *faulty) DeletePrefix(ctx context.Context, dir string) error {
	if f.is(dirSurvivesDeletePrefix) {
		// Deletes the keys but not the directories.
		all, _ := f.Driver.List(ctx, dir, filesystem.ListOptions{Recursive: true, Limit: 1 << 30})
		for _, e := range all.Entries {
			_ = f.Driver.Delete(ctx, e.Key)
		}
		return nil
	}
	if f.is(deleteRawPrefix) {
		all, _ := f.Driver.List(ctx, "", filesystem.ListOptions{Recursive: true, Limit: 1 << 30})
		for _, e := range all.Entries {
			if strings.HasPrefix(e.Key, dir) {
				_ = f.Driver.Delete(ctx, e.Key)
			}
		}
		return nil
	}
	return f.Driver.DeletePrefix(ctx, dir)
}

func (f *faulty) DeleteKeys(ctx context.Context, keys []string) error {
	if f.is(deleteFirstKeyOnly) {
		return f.Driver.DeleteKeys(ctx, keys[:1])
	}
	return f.Driver.DeleteKeys(ctx, keys)
}

func (f *faulty) CompleteMultipart(ctx context.Context, key, id string, parts []filesystem.Part) (filesystem.Entry, error) {
	if f.is(reversedParts) {
		listed, err := f.Driver.ListParts(ctx, key, id)
		if err != nil {
			return filesystem.Entry{}, err
		}
		var buf bytes.Buffer
		for i := len(listed) - 1; i >= 0; i-- {
			buf.WriteString(strings.Repeat("?", int(listed[i].Size)))
		}
		_ = f.Driver.AbortMultipart(ctx, key, id)
		return f.Driver.Write(ctx, key, &buf, filesystem.WriteOptions{})
	}
	return f.Driver.CompleteMultipart(ctx, key, id, parts)
}

func (f *faulty) ListParts(ctx context.Context, key, id string) ([]filesystem.Part, error) {
	parts, err := f.Driver.ListParts(ctx, key, id)
	if f.is(dropListedPart) && len(parts) > 0 {
		parts = parts[1:]
	}
	return parts, err
}

func (f *faulty) AbortMultipart(ctx context.Context, key, id string) error {
	if f.is(abortNoop) {
		return nil
	}
	return f.Driver.AbortMultipart(ctx, key, id)
}

// Capabilities hide Multipart for the faults of the sweep case that only
// runs on drivers without it.
func (f *faulty) Capabilities() filesystem.Capabilities {
	c := f.Driver.Capabilities()
	if f.is(sweepAllNoMultipart) || f.is(sweepBreaksNoMultipart) || f.is(findReportsObjects) || f.is(findDeletesNoMultipart) || f.is(findFailsNoMultipart) {
		c.Multipart = false
	}
	c.PresignPost = true
	return c
}

// PresignPost is fakePost with the fault applied.
func (f *faulty) PresignPost(_ context.Context, p filesystem.PostPolicy) (filesystem.PresignedPost, error) {
	post := fakePost(p)
	switch {
	case f.is(postBadURL):
		post.URL = "/upload"
	case f.is(postWrongKey) && p.Key != "":
		post.Fields[filesystem.PostFieldKey] = "elsewhere.png"
	case f.is(postNoContentType):
		delete(post.Fields, filesystem.PostFieldContentType)
	case f.is(postNoHeaders):
		delete(post.Fields, filesystem.PostFieldCacheControl)
	case f.is(postLongExpiry):
		post.ExpiresAt = post.ExpiresAt.Add(time.Hour)
	case f.is(postIgnoresPrefix) && p.KeyPrefix != "":
		post.Fields[filesystem.PostFieldKey] = filesystem.PostFilenamePlaceholder
	case f.is(postFails), f.is(postPrefixFails) && p.KeyPrefix != "":
		return filesystem.PresignedPost{}, filesystem.ErrUnavailable
	case f.is(postAlwaysTyped) && p.ContentType == "":
		post.Fields[filesystem.PostFieldContentType] = "application/octet-stream"
	}
	return post, nil
}

// fakePost builds the form an S3-like backend would, without signing.
func fakePost(p filesystem.PostPolicy) filesystem.PresignedPost {
	key := p.Key
	if key == "" {
		key = p.KeyPrefix + "/" + filesystem.PostFilenamePlaceholder
	}
	fields := map[string]string{filesystem.PostFieldKey: key, "policy": "fake", "x-signature": "fake"}
	if p.ContentType != "" {
		fields[filesystem.PostFieldContentType] = p.ContentType
	}
	for name, v := range map[string]string{
		filesystem.PostFieldCacheControl:       p.Headers.CacheControl,
		filesystem.PostFieldContentDisposition: p.Headers.ContentDisposition,
		filesystem.PostFieldContentEncoding:    p.Headers.ContentEncoding,
	} {
		if v != "" {
			fields[name] = v
		}
	}
	return filesystem.PresignedPost{URL: "https://bucket.test/", Fields: fields, ExpiresAt: time.Now().Add(p.Expires)}
}

// posting is the memory driver with a (fake) presigned POST.
type posting struct{ *memory.Driver }

func (p posting) Capabilities() filesystem.Capabilities {
	c := p.Driver.Capabilities()
	c.PresignPost = true
	return c
}

func (posting) PresignPost(_ context.Context, p filesystem.PostPolicy) (filesystem.PresignedPost, error) {
	return fakePost(p), nil
}

func TestSuitePassesOnAPostPresigner(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		d, err := filesystem.NewDisk("posting", posting{memory.New()})
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
}

func (f *faulty) SweepAbandoned(ctx context.Context, before time.Time) (int, error) {
	switch {
	case f.is(sweepAllNoMultipart):
		return 0, f.Driver.DeletePrefix(ctx, "sweep")
	case f.is(sweepBreaksNoMultipart):
		return 0, errors.New("staging directory gone")
	case f.is(sweepNoop):
		return 0, nil
	case f.is(sweepEverything):
		return f.Driver.SweepAbandoned(ctx, before.Add(1000*time.Hour))
	case f.is(sweepObjects):
		n, err := f.Driver.SweepAbandoned(ctx, before)
		if derr := f.Driver.DeletePrefix(ctx, "mp"); derr != nil {
			return n, derr
		}
		return n, err
	}
	return f.Driver.SweepAbandoned(ctx, before)
}

// iotestErrReader fails every read.
type iotestErrReader struct{}

func (iotestErrReader) Read([]byte) (int, error) { return 0, errors.New("range read failed") }

// The refusal case must fail on a disk that does presign POST forms.
func TestPresignPostRefusalCase(t *testing.T) {
	for _, c := range filesystemtest.Cases() {
		if c.Name != "PresignPostUnsupportedIsRefused" {
			continue
		}
		d, err := filesystem.NewDisk("posting", posting{memory.New()})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Check(t.Context(), d); err == nil {
			t.Fatal("case passed on a disk that presigns POST forms")
		}
		return
	}
	t.Fatal("case PresignPostUnsupportedIsRefused not found")
}

// chunkMax is what appendTruncates keeps of an append: less than the
// Append case's concurrent chunks, more than its first two appends.
const chunkMax = 32

// folding is the memory driver over a case-insensitive "filesystem": it
// lower-cases every key and declares CaseInsensitive.
type folding struct{ *memory.Driver }

func (f folding) Capabilities() filesystem.Capabilities {
	c := f.Driver.Capabilities()
	c.CaseInsensitive = true
	return c
}

func (f folding) Write(ctx context.Context, key string, body io.Reader, o filesystem.WriteOptions) (filesystem.Entry, error) {
	return f.Driver.Write(ctx, strings.ToLower(key), body, o)
}

func (f folding) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	return f.Driver.Read(ctx, strings.ToLower(key), rng)
}

func (f folding) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	return f.Driver.Stat(ctx, strings.ToLower(key))
}

func TestLetterCaseOnACaseInsensitiveDriver(t *testing.T) {
	for _, c := range filesystemtest.Cases() {
		if c.Name != "LetterCase" {
			continue
		}
		d, err := filesystem.NewDisk("folding", folding{memory.New()})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Check(t.Context(), d); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatal("case LetterCase not found")
}
