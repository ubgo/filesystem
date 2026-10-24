package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/upload"
)

// Settings of the steps in this file.
const (
	// shardDepth is how many two-character directory levels ShardedKey
	// puts in front of the random name.
	shardDepth = 2
	// freeLimit and paidLimit are the per-file limits of two plans.
	freeLimit = 64
	paidLimit = maxUpload
	// planHeader stands in for what authentication tells the handler
	// about the caller; a real app reads the user from the session.
	planHeader = "X-Plan"
	// The plans planHeader can carry in this example.
	planFree    = "free"
	planPaid    = "paid"
	planSpammer = "spammer"
	// retryAfter is how long a rate-limited client is told to wait.
	retryAfter = time.Minute
	// requestTotal caps the bytes of all files of one form post together.
	requestTotal = 60
	// cacheDir holds uploads until their record is saved.
	cacheDir = "cache"
	// hugeBody is the size of a request body far over every limit.
	hugeBody = 8 << 20
	// readBound is the most the handler should read of that body before
	// refusing it: one buffer, not the whole body.
	readBound = 128 << 10
	// slowMobileLink is a ticket lifetime long enough for a phone on a
	// poor connection to start a large upload.
	slowMobileLink = 24 * time.Hour
)

// layouts shows two ways to choose keys. ShardedKey spreads random names
// over two-character directories, so a local disk never holds one huge
// directory. NameKey keeps the client's file name (made safe), and the
// Collision field decides what a second upload of the same name does.
// ds:def id=layouts-fecj8ewm
func layouts(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	sharded := policy
	sharded.Key = upload.ShardedKey("photos", shardDepth)
	res, err := upload.Receive(ctx, disk, strings.NewReader(pngFile), "a.png", sharded)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "sharded: %d directory levels under photos/\n", strings.Count(path.Dir(res.Key), "/"))
	for _, c := range []upload.Collision{upload.CollisionRename, upload.CollisionFail} {
		named := policy
		named.Key = upload.NameKey("drive/" + string(c))
		named.Collision = c
		var got []string
		for range 2 {
			res, err := upload.Receive(ctx, disk, strings.NewReader(pngFile), "photo.png", named)
			if err != nil {
				got = append(got, string(filesystem.CodeOf(err)))
				continue
			}
			got = append(got, res.Key)
		}
		fmt.Fprintf(out, "collision %s: %s\n", c, strings.Join(got, ", "))
	}
	return nil
}

// encoded stores files sent as text: a base64 body from a JSON API and a
// data URI from a canvas export. Both decode while streaming and are
// Receive in every other respect, so the data URI's declared type is
// ignored and the bytes decide.
// ds:def id=encoded-kb86vpvx
func encoded(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(pngFile))
	res, err := upload.FromBase64(ctx, disk, b64, "avatar.png", policy)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "base64: %s, %d bytes\n", res.ContentType, res.Size)
	res, err = upload.FromDataURI(ctx, disk, "data:text/plain;base64,"+b64, "paste.png", policy)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "data URI declared text/plain: stored as %s\n", res.ContentType)
	_, err = upload.FromBase64(ctx, disk, "not base64!", "x.png", policy)
	fmt.Fprintf(out, "bad base64: %s\n", filesystem.CodeOf(err))
	return nil
}

// sizeLimits shows the size checks besides MaxSize: MinSize refuses empty
// files, MaxTotal caps all the files of one request together, and the
// handler caps the request body itself, so an oversized post is refused
// without reading it to the end.
// ds:def id=sizelimits-hkgs2nwg
func sizeLimits(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	nonEmpty := policy
	nonEmpty.MinSize = 1
	_, err = upload.Receive(ctx, disk, strings.NewReader(""), "empty.png", nonEmpty)
	fmt.Fprintf(out, "empty file: %s\n", filesystem.CodeOf(err))
	two := upload.Handler(disk, policy, upload.HandlerOptions{MaxFiles: 2, MaxTotal: requestTotal})
	status, _, err := postForm(ctx, two, formFile{"a.png", pngFile}, formFile{"b.png", pngFile + pngFile})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "two files over MaxTotal %d: %d\n", requestTotal, status)
	small := policy
	small.MaxSize = freeLimit
	one := upload.Handler(disk, small, upload.HandlerOptions{})
	req := formRequest(ctx, "", formFile{"big.png", pngFile + strings.Repeat("x", hugeBody)})
	body := &countingReader{r: req.Body}
	req.Body = io.NopCloser(body)
	rec := httptest.NewRecorder()
	one.ServeHTTP(rec, req)
	fmt.Fprintf(out, "body of %d bytes: %d after reading under %d\n", hugeBody, rec.Code, readBound)
	if body.n >= readBound {
		return fmt.Errorf("the handler read %d bytes of an oversized body", body.n)
	}
	return nil
}

// countingReader counts the bytes read through it, to show how much of an
// oversized request the handler reads before refusing it.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// perRequest chooses the policy for each request before any byte of the
// body is read: the caller's plan sets the size limit, and a rate limiter
// refuses early with unavailable and a Retry-After, which the client
// receives as 503.
// ds:def id=perrequest-jrzefwkt
func perRequest(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	limited := map[string]bool{planSpammer: true}
	resolve := func(r *http.Request, p upload.Policy) (upload.Policy, error) {
		if limited[r.Header.Get(planHeader)] {
			slow := filesystem.NewError(filesystem.CodeUnavailable, errors.New("too many uploads"))
			return p, slow.WithRetryAfter(retryAfter)
		}
		p.MaxSize = freeLimit
		if r.Header.Get(planHeader) == planPaid {
			p.MaxSize = paidLimit
		}
		return p, nil
	}
	h := upload.Handler(disk, policy, upload.HandlerOptions{Resolve: resolve})
	body := pngFile + strings.Repeat("x", freeLimit)
	for _, plan := range []string{planFree, planPaid, planSpammer} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, formRequest(ctx, plan, formFile{"photo.png", body}))
		fmt.Fprintf(out, "plan %s: %d retry-after=%q\n", plan, rec.Code, rec.Header().Get("Retry-After"))
	}
	return nil
}

// declared holds a direct upload to what the client said when it asked
// for the ticket: the size and the SHA-256 it computed. ConfirmExpected
// refuses, and deletes, a stored object that is truncated or is another
// file, which Confirm alone would accept as long as its type passed.
// ds:def id=declared-n3nu9wxw
func declared(ctx context.Context, out io.Writer) error {
	b, err := newBucket()
	if err != nil {
		return err
	}
	defer b.srv.Close()
	sum := sha256.Sum256([]byte(pngFile))
	req := upload.TicketRequest{Filename: "photo.png", ContentType: "image/png", Size: int64(len(pngFile)), SHA256: hex.EncodeToString(sum[:])}
	want := upload.Expected{Size: req.Size, SHA256: req.SHA256}
	for _, sent := range []string{pngFile, pngFile[:len(pngFile)-1] + "!"} {
		t, err := upload.Presign(ctx, b.disk, req, policy, ticketLifetime)
		if err != nil {
			return err
		}
		if _, err := put(ctx, t, sent); err != nil {
			return err
		}
		_, err = upload.ConfirmExpected(ctx, b.disk, t.Key, req.Filename, policy, want)
		kept, _ := b.disk.Exists(ctx, t.Key)
		fmt.Fprintf(out, "declared sha256, sent %d bytes: confirm %q, kept %t\n", len(sent), filesystem.CodeOf(err), kept)
	}
	_, err = upload.Presign(ctx, b.disk, req, policy, slowMobileLink)
	fmt.Fprintf(out, "ticket for %s: %v\n", slowMobileLink, err)
	_, err = upload.Presign(ctx, b.disk, req, policy, filesystem.MaxPresignExpiry+time.Hour)
	fmt.Fprintf(out, "ticket past MaxPresignExpiry: %s\n", filesystem.CodeOf(err))
	return nil
}

// cacheThenPromote keeps an upload in a cache area until the record it
// belongs to is saved, then moves it to its permanent key. The key comes
// back from the client, so Promote refuses one outside the cache area.
// ds:def id=cachethenpromote-ywrmuhf4
func cacheThenPromote(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	cached := policy
	cached.Key = upload.RandomKey(cacheDir)
	res, err := upload.Receive(ctx, disk, strings.NewReader(pngFile), "avatar.png", cached)
	if err != nil {
		return err
	}
	e, err := upload.Promote(ctx, disk, cacheDir, res.Key, disk, "avatars/u1.png")
	if err != nil {
		return err
	}
	stillCached, _ := disk.Exists(ctx, res.Key)
	fmt.Fprintf(out, "promoted to %s, still in cache: %t\n", e.Key, stillCached)
	_, err = upload.Promote(ctx, disk, cacheDir, "avatars/u1.png", disk, "avatars/u2.png")
	fmt.Fprintf(out, "promote a key outside the cache: %s\n", filesystem.CodeOf(err))
	return nil
}

// abandoned previews, then sweeps, a multipart upload nobody finished:
// FindAbandoned is the dry run of SweepAbandoned and removes nothing.
// ds:def id=abandoned-hkvv4ks7
func abandoned(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	if _, err := disk.CreateMultipart(ctx, "videos/big.mp4", filesystem.WriteOptions{}); err != nil {
		return err
	}
	cutoff := time.Now().Add(time.Second) // in production: a day or more ago
	found, err := disk.FindAbandoned(ctx, cutoff)
	if err != nil {
		return err
	}
	for _, a := range found {
		fmt.Fprintf(out, "would remove: %s upload of %s\n", a.Kind, a.Key)
	}
	n, err := disk.SweepAbandoned(ctx, cutoff)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "swept %d abandoned upload(s)\n", n)
	return nil
}
