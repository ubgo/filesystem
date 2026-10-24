package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
	"github.com/ubgo/filesystem/adapters/cloudinary"
	"github.com/ubgo/filesystem/adapters/ftp"
	"github.com/ubgo/filesystem/adapters/gcs"
	"github.com/ubgo/filesystem/adapters/s3"
	"github.com/ubgo/filesystem/adapters/sftp"
	"github.com/ubgo/filesystem/adapters/vercelblob"
	"github.com/ubgo/filesystem/adapters/webdav"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/signedurl"
)

// Exit codes.
const (
	exitOK      = 0
	exitFailed  = 1
	exitUsage   = 2
	diskName    = "cli"
	defaultLink = 15 * time.Minute
)

// Environment variables that configure signing for backends that cannot
// sign their own URLs (file, mem, sftp, ftp, webdav). The secret is read
// only from the environment, never from a flag, so it does not appear in
// the process list or the shell history.
const (
	// envSigningSecret holds the HMAC secret, at least
	// signedurl.MinKeyBytes bytes, used as-is: the same bytes the
	// application gives signedurl.Key.Secret.
	envSigningSecret = "FS_SIGNING_SECRET"
	// envSigningKeyID names the key in the URL; it must match the ID the
	// application's verifier holds for that secret.
	envSigningKeyID = "FS_SIGNING_KEY_ID"
	// envSigningBase is the default for url -base: the absolute URL where
	// the application mounts fshttp.Signed.
	envSigningBase = "FS_SIGNING_BASE"
	// defaultSigningKeyID is the key ID when FS_SIGNING_KEY_ID is unset.
	defaultSigningKeyID = "k1"
)

// errUsage marks a command-line mistake (exit 2) rather than a failure.
var errUsage = errors.New("usage")

// command is one subcommand.
type command struct {
	usage   string
	summary string
	run     func(ctx context.Context, c *cli, args []string) error
}

// cli carries the streams and global settings of one invocation.
type cli struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
	json   bool
	mux    *filesystem.Mux
}

var commands = map[string]command{
	"ls":    {"ls [-r] LOCATION", "list a directory (recursively with -r)", cmdLs},
	"stat":  {"stat LOCATION", "show one object", cmdStat},
	"cat":   {"cat LOCATION", "write an object to standard output", cmdCat},
	"put":   {"put [-type MIME] LOCATION [FILE]", "store FILE (or standard input) at LOCATION", cmdPut},
	"cp":    {"cp [-r [-dry-run]] SOURCE TARGET", "copy an object (every object under a directory with -r), on the disk itself when both share a DSN", cmdCp},
	"mv":    {"mv [-r [-dry-run]] SOURCE TARGET", "move an object (every object under a directory with -r), a rename when both share a DSN", cmdMv},
	"rm":    {"rm [-r] LOCATION", "delete an object, or everything under a directory with -r", cmdRm},
	"url":   {"url [-expires D] [-put] [-base URL] [-disk NAME] LOCATION", "print a signed URL (default 15m, GET)", cmdURL},
	"check": {"check DSN", "write, read and delete a probe object", cmdCheck},
	"sync":  {"sync [-delete] [-dry-run] [-checksum] SOURCE TARGET", "copy new and changed objects from one directory to another (and delete what the source lacks with -delete)", cmdSync},
	"sweep": {"sweep -older D [-dry-run] LOCATION", "delete objects under a directory older than D (never the root)", cmdSweep},
}

// commandOrder is the order commands appear in help.
var commandOrder = []string{"ls", "stat", "cat", "put", "cp", "mv", "rm", "sync", "sweep", "url", "check"}

// run executes one invocation. getenv reads the environment (os.Getenv in
// main, a map in tests), so nothing reads process state behind the
// caller's back.
func run(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	global := flag.NewFlagSet("fs", flag.ContinueOnError)
	global.SetOutput(stderr)
	asJSON := global.Bool("json", false, "machine-readable output")
	global.Usage = func() { usage(stderr) }
	if err := global.Parse(args); err != nil {
		return exitUsage
	}
	rest := global.Args()
	if len(rest) == 0 || rest[0] == "help" {
		usage(stdout)
		if len(rest) == 0 {
			return exitUsage
		}
		return exitOK
	}
	cmd, ok := commands[rest[0]]
	if !ok {
		fmt.Fprintf(stderr, "fs: unknown command %q\n", rest[0])
		usage(stderr)
		return exitUsage
	}
	c := &cli{stdin: stdin, stdout: stdout, stderr: stderr, getenv: getenv, json: *asJSON, mux: newMux()}
	err := cmd.run(ctx, c, rest[1:])
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errUsage):
		fmt.Fprintf(stderr, "usage: fs %s\n", cmd.usage)
		return exitUsage
	}
	var partial *partialError
	if c.json && errors.As(err, &partial) {
		// The command's JSON result already lists every failure.
		return exitFailed
	}
	if c.json {
		_ = json.NewEncoder(stdout).Encode(errorOutput{Error: err.Error(), Code: filesystem.CodeOf(err)})
	} else {
		fmt.Fprintln(stderr, "fs:", err)
	}
	return exitFailed
}

func newMux() *filesystem.Mux {
	m := filesystem.NewMux()
	_ = m.Register(local.Scheme, local.Opener)
	_ = m.Register(memory.Scheme, memory.Opener)
	_ = m.Register(s3.Scheme, s3.Opener)
	_ = m.Register(gcs.Scheme, gcs.Opener)
	_ = m.Register(azure.Scheme, azure.Opener)
	_ = m.Register(sftp.Scheme, sftp.Opener)
	_ = m.Register(ftp.Scheme, ftp.Opener)
	_ = m.Register(ftp.SchemeSecure, ftp.Opener)
	_ = m.Register(ftp.SchemeImplicit, ftp.Opener)
	_ = m.Register(webdav.Scheme, webdav.Opener)
	_ = m.Register(webdav.SchemeSecure, webdav.Opener)
	_ = m.Register(cloudinary.Scheme, cloudinary.Opener)
	_ = m.Register(vercelblob.Scheme, vercelblob.Opener)
	return m
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "fs: work with any disk ubgo/filesystem can open.")
	fmt.Fprintln(w, "\nUsage: fs [-json] COMMAND ARGS\n\nA LOCATION is a DSN whose fragment is the key: 'file:///srv/files#docs/a.pdf'.\n\nCommands:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, name := range commandOrder {
		fmt.Fprintf(tw, "  %s\t%s\n", commands[name].usage, commands[name].summary)
	}
	_ = tw.Flush()
	fmt.Fprintln(w, "\nSchemes: file, mem, s3, gs, azblob, sftp, ftp, ftps, ftps-implicit, webdav, webdavs, cloudinary, vercelblob. url on file, mem, sftp, ftp and webdav signs with "+envSigningSecret+" (and "+envSigningKeyID+", "+envSigningBase+").")
	fmt.Fprintln(w, "\nExamples:\n  fs ls -r 'file:///srv/files#avatars'\n  fs cp 'file:///srv/files#a.png' 's3://media?region=auto&endpoint=https://acct.r2.cloudflarestorage.com#a.png'\n  fs -json stat 's3://media?region=us-east-1#reports/q3.pdf'")
}

// open parses LOCATION into a disk named diskName and a key.
func (c *cli) open(ctx context.Context, loc string) (*filesystem.Disk, string, error) {
	return c.openAs(ctx, diskName, loc)
}

// openAs is open with a chosen disk name and extra Disk options.
func (c *cli) openAs(ctx context.Context, name, loc string, opts ...filesystem.Option) (*filesystem.Disk, string, error) {
	dsn, key := splitLocation(loc)
	d, err := c.mux.Open(ctx, name, dsn, opts...)
	if err != nil {
		return nil, "", err
	}
	return d, key, nil
}

// splitLocation separates a location's DSN from its key (the fragment).
func splitLocation(loc string) (dsn, key string) {
	dsn, key, _ = strings.Cut(loc, "#")
	return dsn, key
}

// flags parses a subcommand's flags and requires exactly n positional args
// (or between n and maxArgs when maxArgs > n).
func flags(name string, args []string, n, maxArgs int, define func(*flag.FlagSet)) ([]string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if define != nil {
		define(fs)
	}
	if err := fs.Parse(args); err != nil {
		return nil, errUsage
	}
	pos := fs.Args()
	if len(pos) < n || len(pos) > max(n, maxArgs) {
		return nil, errUsage
	}
	return pos, nil
}

// JSON output shapes.
type (
	entryOutput struct {
		Key         string            `json:"key"`
		Dir         bool              `json:"dir,omitempty"`
		Size        int64             `json:"size"`
		ModTime     *time.Time        `json:"mod_time,omitempty"`
		ContentType string            `json:"content_type,omitempty"`
		ETag        string            `json:"etag,omitempty"`
		SHA256      string            `json:"sha256,omitempty"`
		Visibility  string            `json:"visibility,omitempty"`
		Metadata    map[string]string `json:"metadata,omitempty"`
	}
	urlOutput struct {
		Method    string              `json:"method"`
		URL       string              `json:"url"`
		Headers   map[string][]string `json:"headers,omitempty"`
		ExpiresAt *time.Time          `json:"expires_at,omitempty"`
	}
	okOutput struct {
		OK bool `json:"ok"`
	}
	errorOutput struct {
		Error string          `json:"error"`
		Code  filesystem.Code `json:"code"`
	}
)

func toOutput(e filesystem.Entry) entryOutput {
	o := entryOutput{Key: e.Key, Dir: e.IsDir, Size: e.Size, ContentType: e.ContentType, ETag: e.ETag, SHA256: e.SHA256, Visibility: string(e.Visibility), Metadata: e.Metadata}
	if !e.ModTime.IsZero() {
		t := e.ModTime.UTC()
		o.ModTime = &t
	}
	return o
}

func emit[T entryOutput | []entryOutput | urlOutput | okOutput | treeOutput | syncOutput | sweepOutput](c *cli, v T) error {
	enc := json.NewEncoder(c.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (c *cli) entry(e filesystem.Entry) error {
	if c.json {
		return emit(c, toOutput(e))
	}
	fmt.Fprintf(c.stdout, "key:          %s\nsize:         %d\nmodified:     %s\ncontent type: %s\netag:         %s\nvisibility:   %s\n",
		e.Key, e.Size, e.ModTime.UTC().Format(time.RFC3339), e.ContentType, e.ETag, e.Visibility)
	if e.SHA256 != "" {
		fmt.Fprintf(c.stdout, "sha256:       %s\n", e.SHA256)
	}
	return nil
}

func (c *cli) ok() error {
	if c.json {
		return emit(c, okOutput{OK: true})
	}
	return nil
}

func cmdLs(ctx context.Context, c *cli, args []string) error {
	var recursive bool
	pos, err := flags("ls", args, 1, 1, func(f *flag.FlagSet) { f.BoolVar(&recursive, "r", false, "recursive") })
	if err != nil {
		return err
	}
	d, dir, err := c.open(ctx, pos[0])
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	var out []entryOutput
	tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	for e, err := range d.List(ctx, dir, filesystem.ListOptions{Recursive: recursive}) {
		if err != nil {
			return err
		}
		if c.json {
			out = append(out, toOutput(e))
			continue
		}
		if e.IsDir {
			fmt.Fprintf(tw, "%s\t%s\t%s/\t\n", "-", "-", e.Key)
		} else {
			fmt.Fprintf(tw, "%d\t%s\t%s\t\n", e.Size, e.ModTime.UTC().Format(time.DateTime), e.Key)
		}
	}
	if c.json {
		if out == nil {
			out = []entryOutput{}
		}
		return emit(c, out)
	}
	return tw.Flush()
}

func cmdStat(ctx context.Context, c *cli, args []string) error {
	pos, err := flags("stat", args, 1, 1, nil)
	if err != nil {
		return err
	}
	d, key, err := c.open(ctx, pos[0])
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	e, err := d.Stat(ctx, key)
	if err != nil {
		return err
	}
	return c.entry(e)
}

func cmdCat(ctx context.Context, c *cli, args []string) error {
	pos, err := flags("cat", args, 1, 1, nil)
	if err != nil {
		return err
	}
	d, key, err := c.open(ctx, pos[0])
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	rc, err := d.Read(ctx, key)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	_, err = io.Copy(c.stdout, rc)
	return err
}

func cmdPut(ctx context.Context, c *cli, args []string) error {
	var contentType string
	pos, err := flags("put", args, 1, 2, func(f *flag.FlagSet) { f.StringVar(&contentType, "type", "", "content type (detected when empty)") })
	if err != nil {
		return err
	}
	d, key, err := c.open(ctx, pos[0])
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	body := c.stdin
	if len(pos) == 2 && pos[1] != "-" {
		f, err := os.Open(pos[1])
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		body = f
	}
	e, err := d.Write(ctx, key, body, filesystem.WriteOptions{ContentType: contentType})
	if err != nil {
		return err
	}
	return c.entry(e)
}

func cmdCp(ctx context.Context, c *cli, args []string) error { return transfer(ctx, c, args, false) }

func cmdMv(ctx context.Context, c *cli, args []string) error { return transfer(ctx, c, args, true) }

// transfer copies or moves one object, or with -r every object under a
// directory. Two locations with the same DSN are one disk, so the operation
// runs on it (Disk.Copy, Disk.Move: a server-side copy or a rename where the
// backend has one); otherwise each location is its own disk and the bytes
// stream between them.
func transfer(ctx context.Context, c *cli, args []string, move bool) error {
	var recursive, dryRun bool
	pos, err := flags("cp", args, 2, 2, func(f *flag.FlagSet) {
		f.BoolVar(&recursive, "r", false, "every object under the source directory")
		f.BoolVar(&dryRun, "dry-run", false, "with -r, print what would be transferred and change nothing")
	})
	if err != nil {
		return err
	}
	if dryRun && !recursive {
		return errUsage
	}
	if recursive {
		return transferTree(ctx, c, pos[0], pos[1], move, dryRun)
	}
	srcDSN, _ := splitLocation(pos[0])
	dstDSN, _ := splitLocation(pos[1])
	if srcDSN == dstDSN {
		return transferWithin(ctx, c, pos[0], pos[1], move)
	}
	src, sk, err := c.open(ctx, pos[0])
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	dst, dk, err := c.open(ctx, pos[1])
	if err != nil {
		return err
	}
	defer func() { _ = dst.Close() }()
	op := filesystem.CopyAcross
	if move {
		op = filesystem.MoveAcross
	}
	e, err := op(ctx, src, sk, dst, dk)
	if err != nil {
		return err
	}
	return c.entry(e)
}

// transferWithin copies or moves between two keys of one disk.
func transferWithin(ctx context.Context, c *cli, from, to string, move bool) error {
	d, sk, err := c.open(ctx, from)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	_, dk := splitLocation(to)
	op := d.Copy
	if move {
		op = d.Move
	}
	e, err := op(ctx, sk, dk)
	if err != nil {
		return err
	}
	return c.entry(e)
}

func cmdRm(ctx context.Context, c *cli, args []string) error {
	var recursive bool
	pos, err := flags("rm", args, 1, 1, func(f *flag.FlagSet) { f.BoolVar(&recursive, "r", false, "delete everything under the directory") })
	if err != nil {
		return err
	}
	d, key, err := c.open(ctx, pos[0])
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if recursive {
		err = d.DeletePrefix(ctx, key)
	} else {
		err = d.Delete(ctx, key)
	}
	if err != nil {
		return err
	}
	return c.ok()
}

func cmdURL(ctx context.Context, c *cli, args []string) error {
	var expires time.Duration
	var put bool
	var base, name string
	pos, err := flags("url", args, 1, 1, func(f *flag.FlagSet) {
		f.DurationVar(&expires, "expires", defaultLink, "lifetime of the URL")
		f.BoolVar(&put, "put", false, "sign an upload instead of a download")
		f.StringVar(&base, "base", c.getenv(envSigningBase), "where the application serves signed URLs (fshttp.Signed)")
		f.StringVar(&name, "disk", diskName, "the disk name the application serves the location under")
	})
	if err != nil {
		return err
	}
	signer, err := c.signer(base)
	if err != nil {
		return err
	}
	var opts []filesystem.Option
	if signer != nil {
		opts = append(opts, filesystem.WithURLSigner(signer))
	}
	d, key, err := c.openAs(ctx, name, pos[0], opts...)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	method := filesystem.MethodGet
	if put {
		method = filesystem.MethodPut
	}
	req, err := d.SignedURL(ctx, key, filesystem.PresignOptions{Method: method, Expires: expires})
	if err != nil {
		if signer == nil && filesystem.CodeOf(err) == filesystem.CodeUnsupported {
			return fmt.Errorf("%w (set %s and -base or %s to sign for a backend that cannot sign its own URLs)", err, envSigningSecret, envSigningBase)
		}
		return err
	}
	if c.json {
		exp := req.ExpiresAt.UTC()
		return emit(c, urlOutput{Method: string(req.Method), URL: req.URL, Headers: req.Header, ExpiresAt: &exp})
	}
	fmt.Fprintln(c.stdout, req.URL)
	return nil
}

// signer builds the URL signer from the environment, or returns nil when
// no secret is set or no base is known (url then relies on the backend's
// own signing, and says what to set when there is none).
func (c *cli) signer(base string) (*signedurl.Signer, error) {
	secret := c.getenv(envSigningSecret)
	if secret == "" || base == "" {
		return nil, nil
	}
	id := c.getenv(envSigningKeyID)
	if id == "" {
		id = defaultSigningKeyID
	}
	return signedurl.New(base, []signedurl.Key{{ID: id, Secret: []byte(secret)}})
}

func cmdCheck(ctx context.Context, c *cli, args []string) error {
	pos, err := flags("check", args, 1, 1, nil)
	if err != nil {
		return err
	}
	d, _, err := c.open(ctx, pos[0])
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Check(ctx); err != nil {
		return err
	}
	if !c.json {
		fmt.Fprintln(c.stdout, "ok")
	}
	return c.ok()
}
