package ftp

import (
	"context"
	"errors"
	"io"
	"net"
	"net/textproto"
	"sync"
	"time"

	"github.com/jlaffaye/ftp"

	"github.com/ubgo/filesystem"
)

// conn is one control connection and the socket under it, kept so a
// cancelled context can close the socket (jlaffaye/ftp commands take no
// context) and so a broken connection is never handed out again.
type conn struct {
	sc   *ftp.ServerConn
	raw  net.Conn
	idle time.Time // when it was returned to the pool
}

// dialer opens one logged-in control connection within ctx.
type dialer func(ctx context.Context) (*conn, error)

// pool holds up to size connections, since an FTP control connection runs
// one command at a time. Idle connections are reused newest first; one
// idle longer than idleCheck is checked with NOOP before use, because
// servers drop idle sessions (vsftpd after 300 s by default).
type pool struct {
	dial      dialer
	idleCheck time.Duration
	slots     chan struct{}

	mu     sync.Mutex
	idle   []*conn
	closed bool
}

func newPool(dial dialer, size int, idleCheck time.Duration) *pool {
	return &pool{dial: dial, idleCheck: idleCheck, slots: make(chan struct{}, size)}
}

// get returns a connection, waiting for a free slot no longer than ctx
// allows.
func (p *pool) get(ctx context.Context) (*conn, error) {
	select {
	case p.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, filesystem.NewError(filesystem.CodeUnavailable, ctx.Err())
	}
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			<-p.slots
			return nil, filesystem.Errorf(filesystem.CodeUnavailable, "ftp: driver is closed")
		}
		var c *conn
		if n := len(p.idle); n > 0 {
			c, p.idle = p.idle[n-1], p.idle[:n-1]
		}
		p.mu.Unlock()
		if c == nil {
			break
		}
		if time.Since(c.idle) < p.idleCheck || c.sc.NoOp() == nil {
			return c, nil
		}
		c.close()
	}
	c, err := p.dial(ctx)
	if err != nil {
		<-p.slots
		return nil, err
	}
	return c, nil
}

// put returns c to the pool, or closes it when it is broken or the pool is
// closed, and frees its slot.
func (p *pool) put(c *conn, broken bool) {
	defer func() { <-p.slots }()
	p.mu.Lock()
	if broken || p.closed {
		p.mu.Unlock()
		c.close()
		return
	}
	c.idle = time.Now()
	p.idle = append(p.idle, c)
	p.mu.Unlock()
}

// close closes the idle connections; those in use close when returned.
func (p *pool) close() {
	p.mu.Lock()
	idle := p.idle
	p.idle, p.closed = nil, true
	p.mu.Unlock()
	for _, c := range idle {
		c.close()
	}
}

// close ends the session politely, then the socket whatever QUIT did.
func (c *conn) close() {
	_ = c.sc.Quit()
	_ = c.raw.Close()
}

// run runs fn on a pooled connection, bound to ctx: when ctx ends the
// socket is closed, which fails the command in progress. The connection
// goes back to the pool unless the error says it is unusable.
func (p *pool) run(ctx context.Context, fn func(*conn) error) error {
	c, err := p.get(ctx)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.raw.Close() })
	err = fn(c)
	cancelled := !stop()
	p.put(c, cancelled || brokenBy(err))
	if cancelled {
		return ctx.Err()
	}
	return classify(err)
}

// brokenBy reports whether err, as returned inside run, leaves the
// connection unusable: a 421 reply (the server is closing the session), or
// anything that is neither a reply nor the driver's own verdict (a network
// failure, a closed socket, a body that failed mid-transfer).
func brokenBy(err error) bool {
	var te *textproto.Error
	switch {
	case err == nil:
		return false
	case errors.As(err, &te):
		return te.Code == ftp.StatusNotAvailable
	}
	return !errors.As(err, new(*filesystem.Error))
}

// classify maps an FTP reply to a filesystem.Code, in place of the bare
// textproto error; anything that is not a reply is a lost connection
// (CodeUnavailable), except the caller's own body failure. Errors already
// classified are kept.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var fe *filesystem.Error
	if errors.As(err, &fe) || errors.As(err, new(bodyError)) {
		return err
	}
	var te *textproto.Error
	if !errors.As(err, &te) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return filesystem.NewError(filesystem.CodeUnavailable, err)
	}
	switch te.Code {
	case ftp.StatusFileUnavailable:
		return filesystem.NewError(filesystem.CodeNotFound, err)
	case ftp.StatusNotLoggedIn:
		return filesystem.NewError(filesystem.CodePermission, err)
	case ftp.StatusExceededStorage, statusInsufficientStorage:
		return filesystem.NewError(filesystem.CodeQuotaExceeded, err)
	case ftp.StatusBadFileName:
		return filesystem.NewError(filesystem.CodeInvalidKey, err)
	}
	if te.Code/100 == 4 {
		return filesystem.NewError(filesystem.CodeUnavailable, err)
	}
	return filesystem.NewError(filesystem.CodeUnknown, err)
}

// bodyError is a failure reading the caller's body during Write. It
// unwraps to the original, so errors.Is and filesystem.CodeOf see through
// it, and it breaks the connection (the transfer was cut mid-stream).
type bodyError struct{ err error }

func (b bodyError) Error() string { return b.err.Error() }
func (b bodyError) Unwrap() error { return b.err }

// bodyReader marks the caller's body failures as bodyError.
type bodyReader struct{ r io.Reader }

func (b bodyReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && err != io.EOF {
		err = bodyError{err}
	}
	return n, err
}

// statusInsufficientStorage is RFC 959's 452 (insufficient storage space
// in the system), which jlaffaye/ftp names under another meaning.
const statusInsufficientStorage = 452
