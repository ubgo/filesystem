package filesystem

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
)

// CheckDir is the directory Check writes its probe object into. It is a
// fixed, recognisable name so an operator who finds a leftover probe (after
// a crash mid-check) knows what it is and that it is safe to delete.
const CheckDir = ".filesystem-check"

// Check proves the disk works end to end: it writes a small random object,
// reads it back, compares the bytes and deletes it. Run it at start-up so a
// wrong credential or a missing bucket fails the deploy instead of the first
// user upload. A read-only disk cannot be checked this way and reports
// CodeReadOnly.
func (d *Disk) Check(ctx context.Context) error {
	// rand.Text cannot fail (crypto/rand aborts the process instead).
	name := rand.Text()
	probe := []byte(rand.Text() + rand.Text())
	key := CheckDir + "/" + name
	if _, err := d.WriteBytes(ctx, key, probe, WriteOptions{}); err != nil {
		return fmt.Errorf("check disk %s: write: %w", d.name, err)
	}
	got, err := d.ReadAll(ctx, key)
	if err != nil {
		_ = d.Delete(ctx, key)
		return fmt.Errorf("check disk %s: read: %w", d.name, err)
	}
	if !bytes.Equal(got, probe) {
		_ = d.Delete(ctx, key)
		return Errorf(CodeUnknown, "check disk %s: read back %d bytes that differ from the %d written", d.name, len(got), len(probe))
	}
	if err := d.Delete(ctx, key); err != nil {
		return fmt.Errorf("check disk %s: delete: %w", d.name, err)
	}
	return nil
}
