package crypt

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ubgo/filesystem"
)

// Object layout, version 1:
//
//	header (HeaderSize bytes, fixed so sizes convert without a read):
//	  magic "UBGC" | version 1 | chunk size uint32 | object ID (16) |
//	  key ID length uint16 | wrapped key length uint16 | key ID | wrapped key | zero padding
//	chunks: each chunk's plaintext sealed with AES-256-GCM, ciphertext then
//	  16-byte tag; every chunk but the last holds exactly chunk size bytes;
//	  the last holds 0 to chunk size bytes and is sealed as final.
//
// Nonce of chunk i: 4 zero bytes then i as a big-endian uint64. The data
// key is fresh for every write, so a (key, nonce) pair is never reused.
//
// Additional data of chunk i: aadLabel | version | chunk size | object ID |
// i | final flag | storage key. A chunk therefore authenticates only at its
// own position, in its own object (the random object ID and the per-object
// data key), at its own key, and only as final if it was written last, so
// reordering, swapping between objects, moving an object to another key
// and cutting chunks off the end all fail to decrypt.
const (
	// HeaderSize is the fixed length of an object's header.
	HeaderSize = 1024
	// TagSize is the GCM authentication tag added to every chunk.
	TagSize = 16
	// ChunkSize is the plaintext bytes per chunk: a ranged read decrypts at
	// most one extra chunk at each end, and the overhead is 16 bytes per
	// 64 KiB. It is part of the format rather than an option, because Stat
	// and List convert sizes without reading headers; a later format
	// version can change it.
	ChunkSize = 64 << 10
	// DataKeySize is the AES-256 data key length, also the master key
	// length MemoryKMS takes.
	DataKeySize = 32

	formatVersion = 1
	objectIDSize  = 16
	nonceSize     = 12
	// fixedHeader is the header length before the key ID.
	fixedHeader = 4 + 1 + 4 + objectIDSize + 2 + 2
)

var (
	magic    = []byte("UBGC")
	aadLabel = []byte("ubgo/filesystem/crypt\x00")
)

// ErrTampered is wrapped by every error for ciphertext that does not
// authenticate: modified, reordered, truncated, moved from another key, or
// not written by this package. Its code is CodeUnknown.
var ErrTampered = errors.New("crypt: object failed authentication")

// tampered builds the error for content that does not authenticate. It
// never includes key material or plaintext.
func tampered(format string, args ...any) error {
	return filesystem.NewError(filesystem.CodeUnknown, fmt.Errorf("%w: "+format, append([]any{ErrTampered}, args...)...))
}

// header is the decoded object header.
type header struct {
	chunkSize int
	objectID  [objectIDSize]byte
	wrapped   WrappedKey
}

// encode renders h as exactly HeaderSize bytes.
func (h header) encode() ([]byte, error) {
	need := fixedHeader + len(h.wrapped.KeyID) + len(h.wrapped.Ciphertext)
	if need > HeaderSize {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "crypt: key ID and wrapped key take %d bytes, more than the header's %d", need-fixedHeader, HeaderSize-fixedHeader)
	}
	b := make([]byte, HeaderSize)
	copy(b, magic)
	b[4] = formatVersion
	binary.BigEndian.PutUint32(b[5:], uint32(h.chunkSize))
	copy(b[9:], h.objectID[:])
	binary.BigEndian.PutUint16(b[25:], uint16(len(h.wrapped.KeyID)))
	binary.BigEndian.PutUint16(b[27:], uint16(len(h.wrapped.Ciphertext)))
	n := copy(b[fixedHeader:], h.wrapped.KeyID)
	copy(b[fixedHeader+n:], h.wrapped.Ciphertext)
	return b, nil
}

// decodeHeader parses a header, refusing anything this version did not
// write.
func decodeHeader(b []byte) (header, error) {
	if len(b) != HeaderSize || !bytes.Equal(b[:4], magic) {
		return header{}, tampered("no crypt header")
	}
	if b[4] != formatVersion {
		return header{}, tampered("header version %d", b[4])
	}
	var h header
	h.chunkSize = int(binary.BigEndian.Uint32(b[5:]))
	if h.chunkSize != ChunkSize {
		return header{}, tampered("chunk size %d", h.chunkSize)
	}
	copy(h.objectID[:], b[9:])
	idLen := int(binary.BigEndian.Uint16(b[25:]))
	wLen := int(binary.BigEndian.Uint16(b[27:]))
	if fixedHeader+idLen+wLen > HeaderSize {
		return header{}, tampered("header lengths")
	}
	h.wrapped.KeyID = string(b[fixedHeader : fixedHeader+idLen])
	h.wrapped.Ciphertext = bytes.Clone(b[fixedHeader+idLen : fixedHeader+idLen+wLen])
	return h, nil
}

// aadPrefix is the part of every chunk's additional data that does not
// change between chunks.
func (h header) aadPrefix() []byte {
	b := make([]byte, 0, len(aadLabel)+1+4+objectIDSize)
	b = append(b, aadLabel...)
	b = append(b, formatVersion)
	b = binary.BigEndian.AppendUint32(b, uint32(h.chunkSize))
	b = append(b, h.objectID[:]...)
	return b
}

// chunkAAD completes prefix for chunk index, as final or not, at key. It
// copies prefix, which callers share between chunks.
func chunkAAD(prefix []byte, index uint64, final bool, storageKey string) []byte {
	b := binary.BigEndian.AppendUint64(bytes.Clone(prefix), index)
	flag := byte(0)
	if final {
		flag = 1
	}
	b = append(b, flag)
	return append(b, storageKey...)
}

// chunkNonce is the nonce of chunk index.
func chunkNonce(index uint64) []byte {
	n := make([]byte, nonceSize)
	binary.BigEndian.PutUint64(n[nonceSize-8:], index)
	return n
}

// newAEAD returns AES-256-GCM over a data key.
func newAEAD(dek []byte) (cipher.AEAD, error) {
	if len(dek) != DataKeySize {
		return nil, tampered("data key of %d bytes", len(dek))
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// CiphertextSize is the stored size of a plaintext of size bytes: the
// header, the plaintext, and a tag per chunk (at least one chunk, so an
// empty object is HeaderSize+TagSize bytes).
func CiphertextSize(size int64) int64 {
	return HeaderSize + size + TagSize*chunkCount(size)
}

// chunkCount is the number of chunks a plaintext of size bytes takes.
func chunkCount(size int64) int64 {
	if size == 0 {
		return 1
	}
	return (size + ChunkSize - 1) / ChunkSize
}

// PlaintextSize is the inverse of CiphertextSize. It fails with
// ErrTampered for a stored size no plaintext encrypts to, which is how a
// truncated object or one this package did not write shows in Stat and
// List.
func PlaintextSize(stored int64) (int64, error) {
	body := stored - HeaderSize
	if body < TagSize {
		return 0, tampered("stored size %d", stored)
	}
	const sealed = ChunkSize + TagSize
	full, rem := body/sealed, body%sealed
	switch {
	case rem == 0:
		return full * ChunkSize, nil
	case rem < TagSize:
		return 0, tampered("stored size %d", stored)
	}
	return full*ChunkSize + rem - TagSize, nil
}
