package audit

import (
	"context"
	"database/sql"
	"math/rand/v2"
	"strconv"

	"github.com/ubgo/filesystem/records"
)

// DetailBytes is the Detail key of a read's entry: the bytes it handed
// out (records.Served.Bytes), in decimal.
const DetailBytes = "bytes"

// WithAccessLog also logs reads (records.Store.Served: links handed out,
// share links followed, variants served, streams) as entries whose Kind
// is the records.ServeKind and whose Detail has DetailBytes, for the
// reads sample keeps. Reads outnumber changes by far, so the log is off by
// default and sampled when on: pass SampleRate(0.01) to keep one read in a
// hundred, or a func of your own (every read of a confidential file, none
// of thumbnails). A nil sample keeps every read.
func WithAccessLog(sample func(records.Served) bool) Option {
	return func(p *Plugin) {
		if sample == nil {
			sample = func(records.Served) bool { return true }
		}
		p.access = sample
	}
}

// SampleRate returns a sampler for WithAccessLog that keeps each read with
// probability rate: 1 or more keeps all, 0 or less none.
func SampleRate(rate float64) func(records.Served) bool {
	return func(records.Served) bool { return rate >= 1 || (rate > 0 && rand.Float64() < rate) }
}

var _ records.ServeHooks = (*Plugin)(nil)

// OnServed implements records.ServeHooks: with WithAccessLog, it logs the
// reads the sampler keeps, each in a transaction of its own (with
// WithHashChain they take their place in the scope's chain); without, it
// does nothing.
func (p *Plugin) OnServed(ctx context.Context, s *records.Store, sv records.Served) error {
	if p.access == nil || !p.access(sv) {
		return nil
	}
	r, err := p.row(ctx, s, records.Event{
		Kind: records.EventKind(sv.Kind), Scope: sv.Scope, At: sv.At, FileID: sv.FileID, Subject: sv.Subject,
		Detail: map[string]string{DetailBytes: strconv.FormatInt(sv.Bytes, 10)},
	})
	if err != nil {
		return err
	}
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error { return p.write(ctx, s, &r) })
}
