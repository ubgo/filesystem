package crypt

import (
	"bufio"
	"bytes"
	"crypto/cipher"
	"errors"
	"io"
)

// sealer is the io.Reader a write hands the wrapped driver: the header,
// then each chunk of body sealed as it is read, so an object of any size
// is encrypted in one chunk of memory.
type sealer struct {
	src    *bufio.Reader
	aead   cipher.AEAD
	prefix []byte
	key    string
	plain  []byte
	out    bytes.Buffer
	index  uint64
	done   bool
	// read counts the plaintext bytes consumed.
	read int64
}

// newSealer returns a sealer emitting hdr then body's chunks.
func newSealer(body io.Reader, aead cipher.AEAD, hdr header, encoded []byte, key string) *sealer {
	s := &sealer{
		src:    bufio.NewReaderSize(body, ChunkSize),
		aead:   aead,
		prefix: hdr.aadPrefix(),
		key:    key,
		plain:  make([]byte, ChunkSize),
	}
	s.out.Write(encoded)
	return s
}

// Read implements io.Reader.
func (s *sealer) Read(p []byte) (int, error) {
	for s.out.Len() == 0 {
		if s.done {
			return 0, io.EOF
		}
		if err := s.next(); err != nil {
			return 0, err
		}
	}
	return s.out.Read(p)
}

// next seals the next chunk into out. A chunk is final when the body ends
// inside it or right after it, which a one-byte peek tells.
func (s *sealer) next() error {
	n, err := fill(s.src, s.plain)
	final := false
	switch {
	case err == io.EOF:
		final = true
	case err != nil:
		return err
	default:
		if _, perr := s.src.Peek(1); errors.Is(perr, io.EOF) {
			final = true
		} else if perr != nil {
			return perr
		}
	}
	s.read += int64(n)
	s.out.Write(s.aead.Seal(nil, chunkNonce(s.index), s.plain[:n], chunkAAD(s.prefix, s.index, final, s.key)))
	s.index++
	s.done = final
	return nil
}

// fill reads until buf is full or r ends. Unlike io.ReadFull it returns
// io.EOF itself for an end at any point, so an io.ErrUnexpectedEOF the body
// reports (a dropped upload) stays an error and is never taken for the end
// of the plaintext.
func fill(r io.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// opener decrypts a run of sealed chunks from src, starting at chunk
// first. When last is negative the run goes to the end of the object and
// the final chunk is the one src ends after; otherwise chunk last is the
// object's final chunk exactly when lastIsFinal. skip plaintext bytes are
// dropped from the start and at most limit (when non-negative) returned.
type opener struct {
	src         io.ReadCloser
	br          *bufio.Reader
	aead        cipher.AEAD
	prefix      []byte
	key         string
	index       uint64
	last        int64
	lastIsFinal bool
	skip        int64
	limit       int64
	sealed      []byte
	out         []byte
	done        bool
}

// Read implements io.Reader.
func (o *opener) Read(p []byte) (int, error) {
	if o.limit == 0 {
		return 0, io.EOF
	}
	for len(o.out) == 0 {
		if o.done {
			return 0, io.EOF
		}
		if err := o.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, o.out)
	if o.limit >= 0 && int64(n) > o.limit {
		n = int(o.limit)
	}
	o.out = o.out[n:]
	if o.limit >= 0 {
		o.limit -= int64(n)
	}
	return n, nil
}

// next opens the next chunk into out.
func (o *opener) next() error {
	n, err := io.ReadFull(o.br, o.sealed)
	final := false
	switch {
	case errors.Is(err, io.EOF):
		// The run ended on a chunk boundary before a final chunk.
		return tampered("truncated after chunk %d", o.index)
	case errors.Is(err, io.ErrUnexpectedEOF):
		final = true
	case err != nil:
		return err
	case o.last >= 0:
		final = int64(o.index) == o.last && o.lastIsFinal
	default:
		if _, perr := o.br.Peek(1); errors.Is(perr, io.EOF) {
			final = true
		} else if perr != nil {
			return perr
		}
	}
	if n < TagSize {
		return tampered("chunk %d is %d bytes", o.index, n)
	}
	plain, err := o.aead.Open(o.sealed[:0:0], chunkNonce(o.index), o.sealed[:n], chunkAAD(o.prefix, o.index, final, o.key))
	if err != nil {
		return tampered("chunk %d does not authenticate", o.index)
	}
	if o.skip > 0 {
		cut := min(o.skip, int64(len(plain)))
		plain = plain[cut:]
		o.skip -= cut
	}
	o.out = plain
	o.done = final || (o.last >= 0 && int64(o.index) == o.last)
	o.index++
	return nil
}

// Close implements io.Closer.
func (o *opener) Close() error { return o.src.Close() }

// newChunkReader buffers a sealed stream one sealed chunk at a time.
func newChunkReader(r io.Reader) *bufio.Reader {
	return bufio.NewReaderSize(r, ChunkSize+TagSize)
}
