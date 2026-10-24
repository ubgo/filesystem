package upload

import (
	"context"
	"errors"
	"io"
)

// ProgressStep is how many bytes Receive streams between two calls of
// Policy.Progress: often enough for a progress bar, rarely enough that a
// callback per call costs nothing next to the write.
const ProgressStep = 64 << 10

// progressReader counts the bytes Receive streams to the disk and reports
// them to Policy.Progress every ProgressStep bytes and once at the end.
type progressReader struct {
	r        io.Reader
	ctx      context.Context
	info     Info
	report   func(context.Context, Info, int64)
	n        int64
	reported int64
	done     bool
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	switch {
	case errors.Is(err, io.EOF) && !p.done:
		p.done = true
		p.report(p.ctx, p.info, p.n)
	case p.n-p.reported >= ProgressStep:
		p.reported = p.n
		p.report(p.ctx, p.info, p.n)
	}
	return n, err
}
