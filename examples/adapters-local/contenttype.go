package main

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/sniff"
)

// appTypes is an application's own extension table, consulted before the
// fixed one, for formats sniff's table does not list.
var appTypes = map[string]string{".dcm": "application/dicom", ".stl": "model/stl"}

// appContentType is a WithContentType function: the app's own table
// first, then sniff's fixed table, never the host's MIME files, so Stat
// answers the same on a laptop, in CI and in production.
// ds:def id=appcontenttype-v8c3m6tz
func appContentType(key string) string {
	if t, ok := appTypes[strings.ToLower(path.Ext(key))]; ok {
		return t
	}
	return sniff.TypeByExtension(key)
}

// fixedTypes opens a disk whose Stat types come from appContentType and
// prints what Stat and List report for a few keys.
// ds:def id=fixedtypes-p2w7k4na
func fixedTypes(ctx context.Context, out io.Writer, dir string) error {
	drv, err := local.New(dir, local.WithContentType(appContentType))
	if err != nil {
		return err
	}
	disk, err := filesystem.NewDisk("scans", drv)
	if err != nil {
		return err
	}
	defer func() { _ = disk.Close() }()
	for _, k := range []string{"studies/chest.dcm", "notes/readme.md", "notes/data.unknownext"} {
		if _, err := disk.WriteBytes(ctx, k, []byte("x"), filesystem.WriteOptions{}); err != nil {
			return err
		}
		e, err := disk.Stat(ctx, k)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "fixed types: Stat %s -> %q\n", k, e.ContentType)
	}
	for e, err := range disk.List(ctx, "studies", filesystem.ListOptions{}) {
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "fixed types: List %s -> %q\n", e.Key, e.ContentType)
	}
	return nil
}
