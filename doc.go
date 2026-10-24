// Package filesystem is a storage library for Go applications: one portable
// API over local disk, memory, S3-compatible object stores and any backend a
// Driver is written for.
//
// # Layers
//
// The package is the bottom of five layers, each usable without the ones
// above it:
//
//   - Driver: the small interface one backend implements (Stat, Read, Write,
//     Delete, List, Capabilities). Drivers live in their own packages; the
//     ones with third-party dependencies live in their own modules.
//   - Disk: the concrete type applications hold. Every portable behaviour is
//     written once here rather than per driver: key normalisation and
//     traversal rejection, content-type detection, SHA-256 and byte counting
//     on writes, size limits, error classification, URL generation, and the
//     interceptor chain.
//   - Manager: named disks, scoped and mirrored disks, cross-disk copy.
//   - upload (sub-package): how bytes arrive from browsers and phones.
//   - records (separate module): files as database rows attached to models.
//
// # Capabilities
//
// Anything only some backends can do (presigned URLs, server-side copy,
// per-object visibility, multipart uploads, stored metadata) is a capability.
// A driver declares its capabilities in one struct, and NewDisk refuses a
// driver whose declaration does not match the interfaces it implements, so
// no driver can claim a feature it does not have. Disk methods for a missing
// capability either fall back to a portable implementation (Copy, Move,
// DeletePrefix) or return an error whose Code is CodeUnsupported.
//
// # Interceptors
//
// Retry, timeouts, logging, metrics, tracing and read-only enforcement run as
// Interceptors around every Disk operation. They never wrap the driver, so
// they cannot hide its capabilities (the usual failure of Go decorators). See
// the layer sub-package for the built-in ones.
//
// # Keys
//
// Keys are slash-separated, relative and normalised by CleanKey before any
// driver sees them. A key containing a ".." segment is rejected, never
// resolved. Directories do not exist as objects: a "directory" is the set of
// keys sharing a prefix, which is how every object store behaves.
package filesystem
