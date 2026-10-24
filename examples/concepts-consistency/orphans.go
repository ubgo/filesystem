package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// recordedQuery counts the rows that point at one object. Applications may
// read the fs_ tables freely; only writes must go through the Store.
var recordedQuery = fmt.Sprintf("SELECT count(*) FROM %s WHERE %s = ? AND %s = ?",
	records.TableNameFiles, records.ColFileDisk, records.ColFileKey)

// orphanGrace is how old an unrecorded object must be before it counts as
// an orphan: an upload in flight has written its object and not yet
// committed its row, and must not be deleted under it.
const orphanGrace = time.Hour

// findOrphans leaves an object with no row (what a crash between the two
// steps of an upload, or a failed cleanup, can leave), then deletes only
// objects no row points at. upload.Sweep would delete by age alone,
// recorded files included, so it is only for a prefix nothing records.
// A fresh unrecorded object, an upload in flight, is left alone.
// ds:def id=findorphans-4kdhftcx
func findOrphans(ctx context.Context, d *demo, out io.Writer) error {
	cutoff := time.Now().Add(-orphanGrace)
	if _, err := d.store.Upload(ctx, scope, diskName, strings.NewReader(pdf), "kept.pdf", policy); err != nil {
		return err
	}
	if _, err := d.disk.WriteBytes(ctx, uploadDir+"/in-flight.pdf", []byte(pdf), filesystem.WriteOptions{}); err != nil {
		return err
	}
	d.age = 2 * orphanGrace
	_, err := d.disk.WriteBytes(ctx, uploadDir+"/crashed.pdf", []byte(pdf), filesystem.WriteOptions{})
	d.age = 0
	if err != nil {
		return err
	}
	var orphans []string
	for e, err := range d.disk.List(ctx, uploadDir, filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		if !e.ModTime.Before(cutoff) {
			continue // possibly an upload still in flight
		}
		var n int
		if err := d.db.QueryRowContext(ctx, recordedQuery, d.disk.Name(), e.Key).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			orphans = append(orphans, e.Key)
		}
	}
	if err := d.disk.DeleteKeys(ctx, orphans); err != nil {
		return err
	}
	objects, rows, err := d.state(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "orphans deleted %v; objects %d, rows %d\n", orphans, objects, rows)
	return nil
}
