package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ubgo/filesystem"
)

// JSON output of cp -r and mv -r.
type (
	// treeOutput is the whole result. Objects lists what was transferred
	// (with -dry-run, what would be); Failed lists every object that was
	// not, so a script needs only this one document.
	treeOutput struct {
		DryRun  bool            `json:"dry_run,omitempty"`
		Objects []pairOutput    `json:"objects"`
		Failed  []failureOutput `json:"failed"`
	}
	pairOutput struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	// failureOutput is one object that was not transferred. Copied is true
	// when mv wrote the target but could not delete the source: the object
	// then exists in both places.
	failureOutput struct {
		From   string          `json:"from"`
		To     string          `json:"to"`
		Copied bool            `json:"copied,omitempty"`
		Error  string          `json:"error"`
		Code   filesystem.Code `json:"code"`
	}
)

// partialError reports that some objects of a recursive transfer failed.
// Each failure has already been printed (on stderr, or in the JSON result),
// so run prints only this summary, and nothing more under -json.
type partialError struct{ failed, total int }

func (e *partialError) Error() string {
	return fmt.Sprintf("%d of %d objects failed", e.failed, e.total)
}

// transferTree copies or moves every object under the source directory to
// the same relative key under the target directory.
//
// The source is listed in full before anything is written, so the transfer
// works on a snapshot: objects it writes are never listed back, and a move
// never deletes ahead of the listing's cursor. Each object is transferred on
// its own; a failure is recorded and the rest continue. A move deletes a
// source object only after its own copy succeeded, so a failed copy never
// loses data. Within one DSN a target inside the source is refused, because
// the snapshot would then copy into the tree it is reading.
func transferTree(ctx context.Context, c *cli, from, to string, move, dryRun bool) error {
	srcDSN, rawSrc := splitLocation(from)
	dstDSN, rawDst := splitLocation(to)
	srcDir, err := filesystem.CleanDir(rawSrc)
	if err != nil {
		return err
	}
	dstDir, err := filesystem.CleanDir(rawDst)
	if err != nil {
		return err
	}
	same := srcDSN == dstDSN
	if same && within(dstDir, srcDir) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "target %q is inside source %q on the same disk", dstDir, srcDir)
	}
	src, _, err := c.open(ctx, from)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	dst := src
	if !same {
		if dst, _, err = c.open(ctx, to); err != nil {
			return err
		}
		defer func() { _ = dst.Close() }()
	}

	var keys []string
	for e, err := range src.List(ctx, srcDir, filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		keys = append(keys, e.Key)
	}
	if len(keys) == 0 {
		return filesystem.Errorf(filesystem.CodeNotFound, "no objects under %q", srcDir)
	}

	out := treeOutput{DryRun: dryRun, Objects: []pairOutput{}, Failed: []failureOutput{}}
	verb := "copy"
	if move {
		verb = "move"
	}
	for _, key := range keys {
		// No early exit when ctx ends: every remaining object then fails
		// with the context's error and is listed, so the report is whole.
		target := rebase(key, srcDir, dstDir)
		if dryRun {
			out.Objects = append(out.Objects, pairOutput{From: key, To: target})
			if !c.json {
				fmt.Fprintf(c.stdout, "would %s %s -> %s\n", verb, key, target)
			}
			continue
		}
		copied, err := transferOne(ctx, src, dst, key, target, move)
		if err != nil {
			out.Failed = append(out.Failed, failureOutput{From: key, To: target, Copied: copied, Error: err.Error(), Code: filesystem.CodeOf(err)})
			if !c.json {
				if copied {
					fmt.Fprintf(c.stderr, "fs: %s -> %s: copied, but the source was not deleted: %v\n", key, target, err)
				} else {
					fmt.Fprintf(c.stderr, "fs: %s -> %s: %v\n", key, target, err)
				}
			}
			continue
		}
		out.Objects = append(out.Objects, pairOutput{From: key, To: target})
		if !c.json {
			fmt.Fprintf(c.stdout, "%s -> %s\n", key, target)
		}
	}
	if c.json {
		err = emit(c, out)
	}
	if len(out.Failed) > 0 {
		return &partialError{failed: len(out.Failed), total: len(keys)}
	}
	return err
}

// transferOne copies or moves one object and reports whether the target was
// written, which on a failed move means the source still exists beside it.
// On one disk a move is Disk.Move (a rename where the backend has one);
// across disks it is a copy and then a delete, never a delete first.
func transferOne(ctx context.Context, src, dst *filesystem.Disk, key, target string, move bool) (bool, error) {
	if src == dst {
		if !move {
			_, err := src.Copy(ctx, key, target)
			return false, err
		}
		_, err := src.Move(ctx, key, target)
		// Without a native rename, Disk.Move is Copy then Delete; a failed
		// delete means the target was written.
		var fe *filesystem.Error
		return err != nil && errors.As(err, &fe) && fe.Op == filesystem.OpDelete, err
	}
	if _, err := filesystem.CopyAcross(ctx, src, key, dst, target); err != nil {
		return false, err
	}
	if !move {
		return true, nil
	}
	if err := src.Delete(ctx, key); err != nil {
		return true, err
	}
	return true, nil
}

// within reports whether dir is base or below it ("" is the root, which
// contains everything).
func within(dir, base string) bool {
	return base == "" || dir == base || strings.HasPrefix(dir, base+"/")
}

// rebase maps a key listed under srcDir to the same relative key under
// dstDir. A recursive listing yields only keys below srcDir.
func rebase(key, srcDir, dstDir string) string {
	rel := key
	if srcDir != "" {
		rel = strings.TrimPrefix(key, srcDir+"/")
	}
	if dstDir == "" {
		return rel
	}
	return dstDir + "/" + rel
}
