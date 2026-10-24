package filesystemtest

import (
	"context"
	"fmt"
	"io"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/ubgo/filesystem"
)

// opCases are the cases for single-object operations beyond the Driver
// interface (append, compare-and-swap, modification time, tags) and for
// the key contract every driver keeps (conditional reads, letter case,
// characters in keys).
func opCases() []Case {
	return []Case{
		{Name: "Append", Needs: func(c filesystem.Capabilities) bool { return c.Append }, Check: appendConcatenates},
		{Name: "AppendUnsupportedIsRefused", Needs: func(c filesystem.Capabilities) bool { return !c.Append }, Check: appendRefused},
		{Name: "CompareAndSwap", Needs: func(c filesystem.Capabilities) bool { return c.CompareAndSwap }, Check: compareAndSwap},
		{Name: "CompareAndSwapUnsupportedIsRefused", Needs: func(c filesystem.Capabilities) bool { return !c.CompareAndSwap }, Check: compareAndSwapRefused},
		{Name: "SetModTime", Needs: func(c filesystem.Capabilities) bool { return c.SetModTime }, Check: setModTime},
		{Name: "Tags", Needs: func(c filesystem.Capabilities) bool { return c.Tags }, Check: tagsRoundTrip},
		{Name: "StorageClass", Needs: func(c filesystem.Capabilities) bool { return c.StorageClass }, Check: storageClass},
		{Name: "StorageClassUnsupportedIsRefused", Needs: func(c filesystem.Capabilities) bool { return !c.StorageClass }, Check: storageClassRefused},
		{Name: "VersionRead", Needs: func(c filesystem.Capabilities) bool { return c.VersionRead }, Check: versionRead},
		{Name: "ConditionalRead", Check: conditionalRead},
		{Name: "LetterCase", Check: letterCase},
		{Name: "KeyCharactersRoundTrip", Check: keyCharacters},
	}
}

// appenders is how many concurrent appends the Append case races.
const appenders = 8

func appendConcatenates(ctx context.Context, d *filesystem.Disk) error {
	const key = "log/a.txt"
	if _, err := d.Append(ctx, key, strings.NewReader("one"), filesystem.AppendOptions{}); err != nil {
		return fmt.Errorf("append to a missing key: %w", err)
	}
	e, err := d.Append(ctx, key, strings.NewReader("two"), filesystem.AppendOptions{})
	if err != nil {
		return fmt.Errorf("second append: %w", err)
	}
	if e.Size != int64(len("onetwo")) {
		return fmt.Errorf("entry after two appends has size %d, want 6", e.Size)
	}
	if _, err := d.Append(ctx, key, &failingReader{err: errBodyFailed}, filesystem.AppendOptions{}); err == nil {
		return fmt.Errorf("append with a failing body succeeded")
	}
	if err := expect(ctx, d, key, "onetwo"); err != nil {
		return fmt.Errorf("after a failed append: %w", err)
	}
	// A size cap on the resulting object: past it, nothing is appended.
	_, err = d.Append(ctx, key, strings.NewReader("x"), filesystem.AppendOptions{MaxSize: int64(len("onetwo"))})
	if err := expectCode("append past MaxSize", err, filesystem.CodeTooLarge); err != nil {
		return err
	}
	if err := expect(ctx, d, key, "onetwo"); err != nil {
		return fmt.Errorf("after a refused append: %w", err)
	}
	for _, part := range []string{"12345", "678"} {
		if _, err := d.Append(ctx, "log/within.txt", strings.NewReader(part), filesystem.AppendOptions{MaxSize: 10}); err != nil {
			return fmt.Errorf("append within MaxSize: %w", err)
		}
	}
	if err := expect(ctx, d, "log/within.txt", "12345678"); err != nil {
		return err
	}
	_, err = d.Append(ctx, "log/capped.txt", strings.NewReader(strings.Repeat("y", 20)), filesystem.AppendOptions{MaxSize: 10})
	if err := expectCode("new object over MaxSize", err, filesystem.CodeTooLarge); err != nil {
		return err
	}
	if _, err := d.Stat(ctx, "log/capped.txt"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		return fmt.Errorf("an append refused for size created the object (stat err %v)", err)
	}
	// Concurrent appends of distinct chunks: each must land whole.
	var wg sync.WaitGroup
	errs := make(chan error, appenders)
	for i := range appenders {
		wg.Go(func() {
			if _, err := d.Append(ctx, key, strings.NewReader(chunk(i)), filesystem.AppendOptions{}); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		return fmt.Errorf("concurrent append: %w", err)
	}
	got, err := d.ReadAll(ctx, key)
	if err != nil {
		return err
	}
	rest, ok := strings.CutPrefix(string(got), "onetwo")
	if !ok || len(rest) != appenders*chunkLen {
		return fmt.Errorf("object after concurrent appends is %q", got)
	}
	for i := 0; i < len(rest); i += chunkLen {
		if c := rest[i : i+chunkLen]; c != strings.Repeat(c[:1], chunkLen) {
			return fmt.Errorf("concurrent appends interleaved: %q", rest)
		}
	}
	return nil
}

// chunkLen is the size of each concurrent append's chunk.
const chunkLen = 64

// chunk is the i-th concurrent append's body: one letter repeated.
func chunk(i int) string { return strings.Repeat(string(rune('a'+i)), chunkLen) }

func appendRefused(ctx context.Context, d *filesystem.Disk) error {
	_, err := d.Append(ctx, "log/a.txt", strings.NewReader("x"), filesystem.AppendOptions{})
	return expectCode("append without the capability", err, filesystem.CodeUnsupported)
}

func compareAndSwap(ctx context.Context, d *filesystem.Disk) error {
	const key = "cas/doc.json"
	if err := put(ctx, d, key, "v1"); err != nil {
		return err
	}
	v1, err := d.Stat(ctx, key)
	if err != nil {
		return err
	}
	if _, err := d.WriteBytes(ctx, key, []byte("v2"), filesystem.WriteOptions{IfMatch: v1.ETag}); err != nil {
		return fmt.Errorf("write with the current ETag: %w", err)
	}
	_, err = d.WriteBytes(ctx, key, []byte("v3"), filesystem.WriteOptions{IfMatch: v1.ETag})
	if err := expectCode("write with a stale ETag", err, filesystem.CodeConflict); err != nil {
		return err
	}
	if err := expect(ctx, d, key, "v2"); err != nil {
		return fmt.Errorf("after a refused swap: %w", err)
	}
	_, err = d.WriteBytes(ctx, "cas/missing.json", []byte("x"), filesystem.WriteOptions{IfMatch: v1.ETag})
	if err := expectCode("swap of a missing key", err, filesystem.CodeConflict); err != nil {
		return err
	}
	// Two writers that read the same version race: exactly one wins.
	v2, err := d.Stat(ctx, key)
	if err != nil {
		return err
	}
	results := make(chan error, 2)
	for _, body := range []string{"from-a", "from-b-longer"} {
		go func() {
			_, err := d.WriteBytes(ctx, key, []byte(body), filesystem.WriteOptions{IfMatch: v2.ETag})
			results <- err
		}()
	}
	won, lost := 0, 0
	for range 2 {
		switch err := <-results; filesystem.CodeOf(err) {
		case "":
			won++
		case filesystem.CodeConflict:
			lost++
		default:
			return fmt.Errorf("racing swap: %w", err)
		}
	}
	if won != 1 || lost != 1 {
		return fmt.Errorf("racing swaps: %d won and %d lost, want one each", won, lost)
	}
	return nil
}

func compareAndSwapRefused(ctx context.Context, d *filesystem.Disk) error {
	_, err := d.WriteBytes(ctx, "cas.txt", []byte("x"), filesystem.WriteOptions{IfMatch: "etag"})
	return expectCode("IfMatch without the capability", err, filesystem.CodeUnsupported)
}

// setModTimeAt is a time the SetModTime case sets, far from now so no
// clock could produce it.
var setModTimeAt = time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

func setModTime(ctx context.Context, d *filesystem.Disk) error {
	if err := put(ctx, d, "dated.txt", "x"); err != nil {
		return err
	}
	if err := d.SetModTime(ctx, "dated.txt", setModTimeAt); err != nil {
		return fmt.Errorf("set mod time: %w", err)
	}
	e, err := d.Stat(ctx, "dated.txt")
	if err != nil {
		return err
	}
	if !e.ModTime.Truncate(time.Second).Equal(setModTimeAt) {
		return fmt.Errorf("stat reports %v, want %v", e.ModTime, setModTimeAt)
	}
	err = d.SetModTime(ctx, "missing.txt", setModTimeAt)
	return expectCode("set mod time of a missing key", err, filesystem.CodeNotFound)
}

func tagsRoundTrip(ctx context.Context, d *filesystem.Disk) error {
	if err := put(ctx, d, "tagged.txt", "x"); err != nil {
		return err
	}
	want := map[string]string{"team": "media", "retention": "short"}
	if err := d.SetTags(ctx, "tagged.txt", want); err != nil {
		return fmt.Errorf("set tags: %w", err)
	}
	got, err := d.Tags(ctx, "tagged.txt")
	if err != nil {
		return err
	}
	if !maps.Equal(got, want) {
		return fmt.Errorf("tags %v, want %v", got, want)
	}
	if err := d.SetTags(ctx, "tagged.txt", nil); err != nil {
		return err
	}
	if got, err := d.Tags(ctx, "tagged.txt"); err != nil || len(got) != 0 {
		return fmt.Errorf("tags after clearing: %v, %v", got, err)
	}
	_, err = d.Tags(ctx, "missing.txt")
	return expectCode("tags of a missing key", err, filesystem.CodeNotFound)
}

// storageClass moves an object to the class Options name and expects Stat
// to report it.
func storageClass(ctx context.Context, d *filesystem.Disk) error {
	class := optionsFrom(ctx).StorageClass
	if err := put(ctx, d, "classed.txt", "x"); err != nil {
		return err
	}
	if err := d.SetStorageClass(ctx, "classed.txt", class); err != nil {
		return fmt.Errorf("set storage class %q: %w", class, err)
	}
	e, err := d.Stat(ctx, "classed.txt")
	if err != nil {
		return err
	}
	if e.StorageClass != class {
		return fmt.Errorf("stat reports storage class %q, want %q", e.StorageClass, class)
	}
	if err := expect(ctx, d, "classed.txt", "x"); err != nil {
		return fmt.Errorf("content after a class change: %w", err)
	}
	if err := expectCode("empty storage class", d.SetStorageClass(ctx, "classed.txt", ""), filesystem.CodeInvalidArgument); err != nil {
		return err
	}
	return expectCode("storage class of a missing key", d.SetStorageClass(ctx, "missing.txt", class), filesystem.CodeNotFound)
}

func storageClassRefused(ctx context.Context, d *filesystem.Disk) error {
	if err := put(ctx, d, "classed.txt", "x"); err != nil {
		return err
	}
	return expectCode("storage class without the capability", d.SetStorageClass(ctx, "classed.txt", optionsFrom(ctx).StorageClass), filesystem.CodeUnsupported)
}

// versionRead writes two versions, reads the first back by ID, restores
// it as the current content, and checks that no version was lost.
func versionRead(ctx context.Context, d *filesystem.Disk) error {
	const key = "versioned.txt"
	if err := put(ctx, d, key, "one"); err != nil {
		return err
	}
	if err := put(ctx, d, key, "two"); err != nil {
		return err
	}
	vs, err := d.Versions(ctx, key)
	if err != nil {
		return fmt.Errorf("versions: %w", err)
	}
	if len(vs) < 2 || !vs[0].Latest {
		return fmt.Errorf("versions %+v, want at least two, newest first", vs)
	}
	first := vs[1].ID
	rc, err := d.ReadVersion(ctx, key, first)
	if err != nil {
		return fmt.Errorf("read version %s: %w", first, err)
	}
	b, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(b) != "one" {
		return fmt.Errorf("version %s reads %q, %v; want %q", first, b, err, "one")
	}
	if err := d.RestoreVersion(ctx, key, first); err != nil {
		return fmt.Errorf("restore version %s: %w", first, err)
	}
	if err := expect(ctx, d, key, "one"); err != nil {
		return fmt.Errorf("after restoring: %w", err)
	}
	after, err := d.Versions(ctx, key)
	if err != nil || len(after) != len(vs)+1 {
		return fmt.Errorf("restoring left %d versions (%v), want %d", len(after), err, len(vs)+1)
	}
	_, err = d.ReadVersion(ctx, key, "no-such-version")
	if err := expectCode("read of a missing version", err, filesystem.CodeNotFound); err != nil {
		return err
	}
	_, err = d.ReadVersion(ctx, key, "")
	return expectCode("read of an empty version id", err, filesystem.CodeInvalidArgument)
}

// conditionalRead checks Disk.ReadIf, which every disk supports (natively
// or over Stat): a matching ETag or an unchanged time answers not
// modified, anything else returns the body.
func conditionalRead(ctx context.Context, d *filesystem.Disk) error {
	if err := put(ctx, d, "cond.txt", "body"); err != nil {
		return err
	}
	e, err := d.Stat(ctx, "cond.txt")
	if err != nil {
		return err
	}
	unchanged := []filesystem.ReadOptions{{IfNoneMatch: "*"}, {IfModifiedSince: e.ModTime.Add(time.Hour)}}
	if e.ETag != "" {
		unchanged = append(unchanged, filesystem.ReadOptions{IfNoneMatch: e.ETag}, filesystem.ReadOptions{IfNoneMatch: `"` + e.ETag + `"`})
	}
	for _, o := range unchanged {
		if _, _, err := d.ReadIf(ctx, "cond.txt", o); filesystem.CodeOf(err) != filesystem.CodeNotModified {
			return fmt.Errorf("ReadIf %+v: err %v, want not_modified", o, err)
		}
	}
	for _, o := range []filesystem.ReadOptions{{}, {IfNoneMatch: "other-etag"}, {IfModifiedSince: e.ModTime.Add(-time.Hour)}} {
		rc, got, err := d.ReadIf(ctx, "cond.txt", o)
		if err != nil {
			return fmt.Errorf("ReadIf %+v: %w", o, err)
		}
		body, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil || string(body) != "body" || got.Size != int64(len("body")) {
			return fmt.Errorf("ReadIf %+v read %q (entry %+v), %v", o, body, got, err)
		}
	}
	_, _, err = d.ReadIf(ctx, "missing.txt", filesystem.ReadOptions{IfNoneMatch: "x"})
	return expectCode("conditional read of a missing key", err, filesystem.CodeNotFound)
}

// letterCase checks the case contract: keys differing only in case are two
// objects, unless the disk declares CaseInsensitive, when they are one.
func letterCase(ctx context.Context, d *filesystem.Disk) error {
	if err := put(ctx, d, "case/A.txt", "upper"); err != nil {
		return err
	}
	if err := put(ctx, d, "case/a.txt", "lower"); err != nil {
		return err
	}
	if d.Capabilities().CaseInsensitive {
		return expect(ctx, d, "case/A.txt", "lower")
	}
	if err := expect(ctx, d, "case/A.txt", "upper"); err != nil {
		return fmt.Errorf("A.txt and a.txt are not two objects, but the disk does not declare CaseInsensitive: %w", err)
	}
	return expect(ctx, d, "case/a.txt", "lower")
}

// oddKey holds every character some backend cannot store as is (Windows
// and SMB refuse : * ? < > | " ; URLs need % and # escaped) plus a space
// and an accented letter, so a driver that encodes keys (KeyEncoder) must
// decode them back exactly.
const oddKey = `chars/a:b*c?d<e>f|g"h%i#j k é.txt`

func keyCharacters(ctx context.Context, d *filesystem.Disk) error {
	if err := put(ctx, d, oddKey, "odd"); err != nil {
		return err
	}
	if err := expect(ctx, d, oddKey, "odd"); err != nil {
		return err
	}
	var keys []string
	for e, err := range d.List(ctx, "chars", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		keys = append(keys, e.Key)
	}
	if len(keys) != 1 || keys[0] != oddKey {
		return fmt.Errorf("listing returned %q, want exactly %q", keys, oddKey)
	}
	return nil
}
