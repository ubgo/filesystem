package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/layer"
	"github.com/ubgo/filesystem/memory"
)

// errMirrorDown stands for the mirror's backend being unreachable.
var errMirrorDown = errors.New("mirror backend unreachable")

// conditionalWrite shows IfNotExists: the second writer of a key gets
// CodeExists instead of silently replacing the first one's object.
// ds:def id=conditionalwrite-d37uctzq
func conditionalWrite(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("locks", memory.New())
	if err != nil {
		return err
	}
	opts := filesystem.WriteOptions{IfNotExists: true}
	if _, err := disk.WriteBytes(ctx, "jobs/nightly.lock", []byte("worker-1"), opts); err != nil {
		return err
	}
	_, err = disk.WriteBytes(ctx, "jobs/nightly.lock", []byte("worker-2"), opts)
	fmt.Fprintf(out, "second IfNotExists write: %s\n", filesystem.CodeOf(err))
	return nil
}

// mirrorConverges writes through a mirrored disk while the mirror fails.
// The write lands on the primary and reports CodeUnavailable; writing
// again once the mirror is back brings both copies into line.
// ds:def id=mirrorconverges-zsn6p4n9
func mirrorConverges(ctx context.Context, out io.Writer) error {
	down := true
	outage := layer.Guard(func(context.Context, *filesystem.Op) error {
		if down {
			return filesystem.NewError(filesystem.CodeUnavailable, errMirrorDown)
		}
		return nil
	})
	primary, err := filesystem.NewDisk("s3-old", memory.New())
	if err != nil {
		return err
	}
	replica, err := filesystem.NewDisk("s3-new", memory.New(), filesystem.WithInterceptors(outage))
	if err != nil {
		return err
	}
	both, err := filesystem.Mirror("media", primary, replica)
	if err != nil {
		return err
	}
	body := []byte("%PDF-1.7 mirrored")
	_, err = both.WriteBytes(ctx, "a.pdf", body, filesystem.WriteOptions{})
	onPrimary, _ := primary.Exists(ctx, "a.pdf")
	onReplica, _ := replica.Exists(ctx, "a.pdf")
	fmt.Fprintf(out, "mirror down: %s; primary %t, replica %t\n", filesystem.CodeOf(err), onPrimary, onReplica)
	down = false
	if _, err := both.WriteBytes(ctx, "a.pdf", body, filesystem.WriteOptions{}); err != nil {
		return err
	}
	onReplica, _ = replica.Exists(ctx, "a.pdf")
	fmt.Fprintf(out, "written again: replica %t\n", onReplica)
	return nil
}
