package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/upload"
)

// JSON output of sync and sweep.
type (
	// syncOutput is the whole result of sync: the counts, and every
	// action (planned, with -dry-run), failures included.
	syncOutput struct {
		DryRun    bool             `json:"dry_run,omitempty"`
		Copied    int              `json:"copied"`
		Deleted   int              `json:"deleted"`
		Unchanged int              `json:"unchanged"`
		Failed    int              `json:"failed"`
		Items     []syncItemOutput `json:"items"`
	}
	syncItemOutput struct {
		Action string          `json:"action"`
		Key    string          `json:"key"`
		Target string          `json:"target"`
		Size   int64           `json:"size"`
		Error  string          `json:"error,omitempty"`
		Code   filesystem.Code `json:"code,omitempty"`
	}
	// sweepOutput is the result of sweep. Keys lists what a dry run
	// would delete; a real sweep reports only how many it deleted.
	sweepOutput struct {
		DryRun bool     `json:"dry_run,omitempty"`
		Count  int      `json:"count"`
		Keys   []string `json:"keys,omitempty"`
	}
)

// cmdSync copies new and changed objects from SOURCE's directory to
// TARGET's (filesystem.Sync), with -delete removing target objects the
// source no longer has. Two locations with one DSN share one disk, so a
// copy is server-side where the backend has one and overlapping
// directories are refused.
func cmdSync(ctx context.Context, c *cli, args []string) error {
	var opts filesystem.SyncOptions
	pos, err := flags("sync", args, 2, 2, func(f *flag.FlagSet) {
		f.BoolVar(&opts.Delete, "delete", false, "delete target objects the source does not have")
		f.BoolVar(&opts.DryRun, "dry-run", false, "print what would be copied and deleted, and change nothing")
		f.BoolVar(&opts.Checksum, "checksum", false, "compare objects of equal size by reading and hashing both")
	})
	if err != nil {
		return err
	}
	srcDSN, srcDir := splitLocation(pos[0])
	dstDSN, dstDir := splitLocation(pos[1])
	src, _, err := c.open(ctx, pos[0])
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	dst := src
	if srcDSN != dstDSN {
		if dst, _, err = c.open(ctx, pos[1]); err != nil {
			return err
		}
		defer func() { _ = dst.Close() }()
	}
	out := syncOutput{DryRun: opts.DryRun, Items: []syncItemOutput{}}
	opts.OnItem = func(it filesystem.SyncItem) {
		o := syncItemOutput{Action: string(it.Action), Key: it.Key, Target: it.Target, Size: it.Size}
		if it.Err != nil {
			o.Error, o.Code = it.Err.Error(), filesystem.CodeOf(it.Err)
		}
		out.Items = append(out.Items, o)
		if c.json {
			return
		}
		switch {
		case it.Err != nil:
			fmt.Fprintf(c.stderr, "fs: %s %s: %v\n", it.Action, it.Key, it.Err)
		case opts.DryRun:
			fmt.Fprintf(c.stdout, "would %s %s -> %s\n", it.Action, it.Key, it.Target)
		default:
			fmt.Fprintf(c.stdout, "%s %s -> %s\n", it.Action, it.Key, it.Target)
		}
	}
	rep, err := filesystem.Sync(ctx, src, srcDir, dst, dstDir, opts)
	if rep.Failed == 0 && err != nil {
		return err // nothing was done: a listing or argument failure
	}
	out.Copied, out.Deleted, out.Unchanged, out.Failed = rep.Copied, rep.Deleted, rep.Unchanged, rep.Failed
	if c.json {
		if err := emit(c, out); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(c.stdout, "%d copied, %d deleted, %d unchanged, %d failed\n", rep.Copied, rep.Deleted, rep.Unchanged, rep.Failed)
	}
	if rep.Failed > 0 {
		return &partialError{failed: rep.Failed, total: rep.Copied + rep.Deleted + rep.Failed}
	}
	return nil
}

// cmdSweep deletes every object under LOCATION's directory last modified
// more than -older ago (upload.Sweep), or with -dry-run lists them
// (upload.FindStale). Like upload.Sweep it refuses the root of a disk.
func cmdSweep(ctx context.Context, c *cli, args []string) error {
	var older time.Duration
	var dryRun bool
	pos, err := flags("sweep", args, 1, 1, func(f *flag.FlagSet) {
		f.DurationVar(&older, "older", 0, "minimum age of the objects to delete (required)")
		f.BoolVar(&dryRun, "dry-run", false, "print what would be deleted, and change nothing")
	})
	if err != nil {
		return err
	}
	if older <= 0 {
		return errUsage
	}
	d, dir, err := c.open(ctx, pos[0])
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	cutoff := time.Now().Add(-older)
	out := sweepOutput{DryRun: dryRun}
	if dryRun {
		out.Count, err = upload.FindStale(ctx, d, dir, cutoff, func(e filesystem.Entry) {
			out.Keys = append(out.Keys, e.Key)
			if !c.json {
				fmt.Fprintf(c.stdout, "would delete %s\n", e.Key)
			}
		})
	} else {
		out.Count, err = upload.Sweep(ctx, d, dir, cutoff)
	}
	if err != nil {
		return err
	}
	if c.json {
		return emit(c, out)
	}
	if !dryRun {
		fmt.Fprintf(c.stdout, "%d deleted\n", out.Count)
	}
	return nil
}
