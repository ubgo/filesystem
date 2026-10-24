package scan

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
)

// Defaults of Clamd.
const (
	// DefaultTimeout bounds one scan, connection included.
	DefaultTimeout = 2 * time.Minute
	// DefaultChunkSize is the size of the INSTREAM chunks sent to clamd.
	DefaultChunkSize = 64 << 10
)

// clamd protocol words (clamd(8)). Commands start with "z" so that clamd
// answers with NUL-terminated replies.
const (
	cmdInstream   = "zINSTREAM\x00"
	cmdPing       = "zPING\x00"
	replyPong     = "PONG"
	replyStreamOK = "stream: OK"
	suffixFound   = " FOUND"
	suffixError   = " ERROR"
	prefixStream  = "stream: "
	// replySizeLimit is how clamd reports a stream over StreamMaxLength.
	replySizeLimit = "INSTREAM size limit exceeded"
	// maxReplyBytes caps a reply; real replies are a signature name long.
	maxReplyBytes = 4 << 10
)

// Clamd scans with a clamd daemon over TCP ("tcp", "127.0.0.1:3310") or a
// Unix socket ("unix", "/run/clamav/clamd.ctl"), streaming the file with
// the INSTREAM command, so the daemon needs no access to the files. clamd
// refuses streams over its StreamMaxLength (100 MiB by default in current
// ClamAV, 25 MB in old releases); keep it at least as large as the largest
// upload a policy allows, or such uploads fail with CodeTooLarge.
type Clamd struct {
	Network string
	Address string
	// Timeout bounds one scan; 0 means DefaultTimeout.
	Timeout time.Duration
	// ChunkSize of the stream; 0 means DefaultChunkSize.
	ChunkSize int
}

// dial connects with the deadline of ctx and Timeout, whichever is first.
func (c Clamd) dial(ctx context.Context) (net.Conn, context.CancelFunc, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	var d net.Dialer
	conn, err := d.DialContext(ctx, c.Network, c.Address)
	if err != nil {
		cancel()
		return nil, nil, filesystem.NewError(filesystem.CodeUnavailable, err)
	}
	deadline, _ := ctx.Deadline() // always set: ctx has a timeout
	_ = conn.SetDeadline(deadline)
	// Closing the connection when ctx ends unblocks a read or write that
	// is waiting on clamd.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	return conn, func() { stop(); cancel() }, nil
}

// Ping checks that clamd answers, for a readiness probe.
func (c Clamd) Ping(ctx context.Context) error {
	conn, done, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer done()
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, cmdPing) // a failed write shows as a failed read
	reply, err := readReply(conn)
	if err != nil {
		return err
	}
	if reply != replyPong {
		return filesystem.Errorf(filesystem.CodeUnavailable, "scan: clamd answered %q to PING", reply)
	}
	return nil
}

// Scan implements Scanner.
func (c Clamd) Scan(ctx context.Context, r io.Reader) (Verdict, error) {
	conn, done, err := c.dial(ctx)
	if err != nil {
		return Verdict{}, err
	}
	defer done()
	defer func() { _ = conn.Close() }()
	size := c.ChunkSize
	if size <= 0 {
		size = DefaultChunkSize
	}
	readErr, sendErr := stream(conn, r, size)
	if readErr != nil {
		return Verdict{}, readErr // the file could not be read; clamd's answer is moot
	}
	// A failed send is often clamd closing early to report a limit, so
	// its reply decides; the send error matters only without one.
	reply, err := readReply(conn)
	if err != nil {
		if sendErr != nil {
			return Verdict{}, filesystem.NewError(filesystem.CodeUnavailable, sendErr)
		}
		return Verdict{}, err
	}
	return verdict(reply)
}

// stream sends the INSTREAM command, r in length-prefixed chunks, and the
// zero-length chunk that ends the stream. It reports a failure to read r
// apart from a failure to send. A bufio.Writer's errors are sticky, so the
// unchecked writes are checked by the data write and Flush after them.
func stream(w io.Writer, r io.Reader, size int) (readErr, sendErr error) {
	bw := bufio.NewWriterSize(w, size+4)
	_, _ = bw.WriteString(cmdInstream)
	buf := make([]byte, size)
	var prefix [4]byte
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			binary.BigEndian.PutUint32(prefix[:], uint32(n))
			_, _ = bw.Write(prefix[:])
			if _, err := bw.Write(buf[:n]); err != nil {
				return nil, err
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return err, nil
		}
	}
	binary.BigEndian.PutUint32(prefix[:], 0)
	_, _ = bw.Write(prefix[:])
	return nil, bw.Flush()
}

// readReply reads one NUL-terminated reply.
func readReply(r io.Reader) (string, error) {
	reply, err := bufio.NewReader(io.LimitReader(r, maxReplyBytes)).ReadString(0)
	if err != nil && !(errors.Is(err, io.EOF) && reply != "") {
		return "", filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("scan: reading clamd's reply: %w", err))
	}
	return strings.TrimRight(reply, "\x00\n"), nil
}

// verdict parses an INSTREAM reply.
func verdict(reply string) (Verdict, error) {
	switch {
	case reply == replyStreamOK:
		return Verdict{Clean: true}, nil
	case strings.HasPrefix(reply, prefixStream) && strings.HasSuffix(reply, suffixFound):
		return Verdict{Signature: strings.TrimSuffix(strings.TrimPrefix(reply, prefixStream), suffixFound)}, nil
	case strings.HasPrefix(reply, replySizeLimit):
		return Verdict{}, filesystem.Errorf(filesystem.CodeTooLarge, "scan: the file is larger than clamd's StreamMaxLength")
	case strings.HasSuffix(reply, suffixError):
		return Verdict{}, filesystem.Errorf(filesystem.CodeUnavailable, "scan: clamd: %s", reply)
	}
	return Verdict{}, filesystem.Errorf(filesystem.CodeUnavailable, "scan: unexpected clamd reply %q", reply)
}
