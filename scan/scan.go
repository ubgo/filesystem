// Package scan checks uploaded files for malware. It defines the Scanner
// contract, a client for ClamAV's clamd daemon (the free scanner most
// self-hosted products use), and Inspect, which plugs any Scanner into an
// upload.Policy so every upload path scans before it reports success.
//
// Scanning fails closed: a file is accepted only on a clean verdict, so a
// scanner that is down refuses uploads (CodeUnavailable) instead of
// letting them through unscanned. An app that prefers to stay up wraps
// its Scanner and decides there.
package scan

import (
	"context"
	"errors"
	"io"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/upload"
)

// Verdict is the outcome of a scan.
type Verdict struct {
	// Clean is true when nothing was found.
	Clean bool
	// Signature names what was found when Clean is false
	// ("Win.Test.EICAR_HDB-1").
	Signature string
}

// Scanner scans a stream. An error means the scan did not complete; it
// says nothing about the content.
type Scanner interface {
	Scan(ctx context.Context, r io.Reader) (Verdict, error)
}

// ScannerFunc adapts a function to Scanner, for cloud scanning APIs and
// tests.
type ScannerFunc func(ctx context.Context, r io.Reader) (Verdict, error)

// Scan implements Scanner.
func (f ScannerFunc) Scan(ctx context.Context, r io.Reader) (Verdict, error) { return f(ctx, r) }

// Inspect returns an upload.Inspector that scans each stored upload with s:
// an infected file is CodeRejected (and deleted by the upload), a failed
// scan CodeUnavailable unless s already classified its error.
func Inspect(s Scanner) upload.Inspector {
	return func(ctx context.Context, disk *filesystem.Disk, r upload.Result) error {
		rc, err := disk.Read(ctx, r.Key)
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		v, err := s.Scan(ctx, rc)
		if err != nil {
			var fe *filesystem.Error
			if errors.As(err, &fe) {
				return err
			}
			return filesystem.NewError(filesystem.CodeUnavailable, err)
		}
		if !v.Clean {
			return filesystem.Errorf(filesystem.CodeRejected, "scan: the file is infected (%s)", v.Signature)
		}
		return nil
	}
}
