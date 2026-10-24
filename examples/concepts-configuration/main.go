// Command concepts-configuration shows how disks are configured: a Mux
// that knows the drivers the program imports, DSNs with the parameters
// every driver accepts, a Manager built from environment variables, and
// the Manager's own operations (default disk, Check, cross-disk copy).
//
//	go run ./concepts-configuration          (from the examples directory)
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/memory"
)

// envPrefix is the prefix of every storage variable this app reads.
const envPrefix = "STORAGE_"

// cdnBase is the public URL base the media DSN sets; the demo never
// fetches it.
const cdnBase = "https://cdn.example.com"

func main() {
	dir, err := os.MkdirTemp("", "concepts-configuration-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is main without the process, so the test can call it.
func run(ctx context.Context, out io.Writer, dir string) error {
	mux, err := newMux()
	if err != nil {
		return err
	}
	if err := openWithParams(ctx, mux, dir, out); err != nil {
		return err
	}
	refusals(ctx, mux, out)
	mgr, err := fromEnvironment(ctx, mux, dir, out)
	if err != nil {
		return err
	}
	defer func() { _ = mgr.Close() }()
	if err := managerOps(ctx, mgr, out); err != nil {
		return err
	}
	missingVariable(ctx, mux, out)
	return sizes(out)
}

// newMux registers the drivers this program imports. A Mux is a value,
// not a global registry: importing a driver package registers nothing,
// and two Muxes in one process can hold different drivers.
// ds:def id=newmux-6gp3nw3a
func newMux() (*filesystem.Mux, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(local.Scheme, local.Opener); err != nil {
		return nil, err
	}
	if err := mux.Register(memory.Scheme, memory.Opener); err != nil {
		return nil, err
	}
	return mux, nil
}

// openWithParams opens a local disk with the Mux's own parameters: a key
// prefix, a default visibility and a public URL base. The prefix scopes
// the disk, and the public URL gains it too.
// ds:def id=openwithparams-e2jtzbhn
func openWithParams(ctx context.Context, mux *filesystem.Mux, dir string, out io.Writer) error {
	dsn := "file://" + filepath.ToSlash(dir) + "?prefix=tenant-a&visibility=public&public_url=" + cdnBase
	disk, err := mux.Open(ctx, "media", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = disk.Close() }()
	e, err := disk.WriteBytes(ctx, "logo.png", []byte("png"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	u, err := disk.PublicURL(ctx, e.Key)
	if err != nil {
		return err
	}
	_, statErr := os.Stat(filepath.Join(dir, "tenant-a", "logo.png"))
	fmt.Fprintf(out, "key %s, %s, url %s, on disk under tenant-a: %t\n", e.Key, e.Visibility, u, statErr == nil)
	return nil
}

// refusals shows configuration mistakes failing at start-up: a misspelt
// parameter, a scheme nobody registered, and a write to a disk the DSN
// made read-only.
// ds:def id=refusals-2dba5qe4
func refusals(ctx context.Context, mux *filesystem.Mux, out io.Writer) {
	_, err := mux.Open(ctx, "media", "mem://?visiblity=public")
	fmt.Fprintf(out, "typo: %v\n", err)
	_, err = mux.Open(ctx, "media", "s3://AKIA:topsecret@bucket?region=auto")
	fmt.Fprintf(out, "unregistered: %v\n", err)
	ro, err := mux.Open(ctx, "archive", "mem://?readonly=true")
	if err != nil {
		fmt.Fprintf(out, "readonly: %v\n", err)
		return
	}
	_, err = ro.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "readonly write: %s\n", filesystem.CodeOf(err))
}

// fromEnvironment builds every disk from variables, twelve-factor style.
// A map stands in for the process environment; in production pass
// os.LookupEnv.
// ds:def id=fromenvironment-tvhw2dwd
func fromEnvironment(ctx context.Context, mux *filesystem.Mux, dir string, out io.Writer) (*filesystem.Manager, error) {
	env := map[string]string{
		"STORAGE_DISKS":        "media, backups",
		"STORAGE_MEDIA_DSN":    "mem://media?visibility=public",
		"STORAGE_BACKUPS_DSN":  "file://" + filepath.ToSlash(filepath.Join(dir, "backups")),
		"STORAGE_DEFAULT_DISK": "media",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	mgr, err := mux.OpenManager(ctx, envPrefix, lookup)
	if err != nil {
		return nil, err
	}
	def, err := mgr.Default()
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "disks %s, default %s\n", strings.Join(mgr.Names(), ","), def.Name())
	fmt.Fprintf(out, "variable for media-cdn: %s\n", filesystem.EnvName(envPrefix, "media-cdn"))
	return mgr, nil
}

// managerOps checks every disk at start-up, then copies an object from
// one disk to another; CopyAcross streams it because the disks differ.
// ds:def id=managerops-tze3ff3x
func managerOps(ctx context.Context, mgr *filesystem.Manager, out io.Writer) error {
	if err := mgr.Check(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, "check: every disk works")
	media, err := mgr.Disk("media")
	if err != nil {
		return err
	}
	backups, err := mgr.Disk("backups")
	if err != nil {
		return err
	}
	if _, err := media.WriteBytes(ctx, "report.csv", []byte("a,b\n1,2\n"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	e, err := filesystem.CopyAcross(ctx, media, "report.csv", backups, "2026/report.csv")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "copied to backups:%s (%s)\n", e.Key, e.ContentType)
	_, err = mgr.Disk("nope")
	fmt.Fprintf(out, "unknown disk: %s\n", filesystem.CodeOf(err))
	return nil
}

// missingVariable shows that configuration errors name the variable and
// never print a DSN, which usually carries credentials.
// ds:def id=missingvariable-fv6t3a7d
func missingVariable(ctx context.Context, mux *filesystem.Mux, out io.Writer) {
	env := map[string]string{"STORAGE_DISKS": "media,backups", "STORAGE_MEDIA_DSN": "mem://"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	_, err := mux.OpenManager(ctx, envPrefix, lookup)
	fmt.Fprintf(out, "missing: %v\n", err)
}

// sizes reads human sizes from configuration, as an upload limit in an
// environment variable would arrive, and shows the strictness that keeps
// a typo from becoming a limit a thousand times off.
// ds:def id=sizes-82ucppeh
func sizes(out io.Writer) error {
	limit, err := filesystem.ParseSize("10MB")
	if err != nil {
		return err
	}
	binary, err := filesystem.ParseSize("1.5GiB")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "10MB = %d bytes, 1.5GiB = %d bytes, 10*MiB = %d\n", limit, binary, 10*filesystem.MiB)
	for _, typo := range []string{"10mb", "10 MB", "1.5B", "-1KB"} {
		_, err := filesystem.ParseSize(typo)
		fmt.Fprintf(out, "ParseSize(%q): %s\n", typo, filesystem.CodeOf(err))
	}
	return nil
}
