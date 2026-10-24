package upload

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/sniff"
)

// Upload sessions (UP-15): the server issues a token that lets its holder
// upload, until it expires, to one key or under one key prefix, within
// limits narrower than (never wider than) the endpoint's policy. It is
// the shape of Supabase's signed upload URLs, Uploadcare's signed uploads
// and tus' upload expiration: the application decides who may upload
// what when it issues the session (an authenticated API call), and the
// upload itself, from a phone or a third party, carries only the token.
//
// Sessions are recorded in a SessionStore (MemorySessions in this
// process; records/uploadsessions in the database), so they can be
// revoked and swept. A token is the session ID and a 256-bit secret; the
// store keeps only the secret's SHA-256, so a leaked store cannot be
// turned into working tokens. Every redemption re-checks the session on
// the server: its expiry against the Sessions clock (never the store's
// word), its secret in constant time, and its limits against the
// endpoint's policy as it is now, which Receive and Confirm then enforce
// on the bytes. Plug Sessions.Resolver into any upload handler's Resolve.

// HeaderUploadSession is the request header carrying an upload session's
// token. A header, not a query parameter, so the token stays out of
// access logs and Referer headers.
const HeaderUploadSession = "Upload-Session"

// SessionIDPrefix starts every session ID, so an ID says what it is in
// logs.
const SessionIDPrefix = "ups_"

// Session limits.
const (
	// MaxSessionLifetime caps a session's TTL: a session is a window for
	// one upload flow, and a store that is never swept still empties
	// within it.
	MaxSessionLifetime = 7 * 24 * time.Hour
	// MaxSessionTypes caps SessionRequest.Allowed.
	MaxSessionTypes = 32
	// maxTypePatternBytes bounds one MIME pattern; real types are far
	// shorter.
	maxTypePatternBytes = 128
	// sessionSecretBytes is the secret's entropy: 256 bits.
	sessionSecretBytes = 32
	// maxSessionTokenBytes bounds a token before it is parsed: an ID and
	// a base64 secret fit with a wide margin.
	maxSessionTokenBytes = 256
)

// sessionTokenSeparator joins a token's ID and secret. Neither an ID
// (prefix and lowercase base32) nor a base64url secret contains it.
const sessionTokenSeparator = "."

// Session is an upload session as a SessionStore keeps it. Exactly one of
// Key and Prefix is set.
type Session struct {
	// ID identifies the session (SessionIDPrefix and random base32); it
	// is half of the token and is not a secret on its own.
	ID string
	// SecretHash is the hex SHA-256 of the token's secret half: never the
	// secret, which is shown once, by Issue.
	SecretHash string
	// Key, when set, is the one key uploads through the session are
	// stored at (each upload replaces the last, under the policy's
	// Collision).
	Key string
	// Prefix, when set, is the directory uploads are stored under: the
	// policy's key chosen inside it (RandomKey(Prefix) when the policy
	// has no Key).
	Prefix string
	// MaxSize, when positive, is the per-file size cap, at most the
	// policy's; 0 keeps the policy's.
	MaxSize int64
	// Allowed, when set, narrows the policy's types: each pattern lies
	// within the policy's Allowed. nil keeps the policy's.
	Allowed []string
	// CreatedAt and ExpiresAt bound the session; it is refused from
	// ExpiresAt on.
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Expired reports whether s has expired at now.
func (s Session) Expired(now time.Time) bool { return !now.Before(s.ExpiresAt) }

// SessionRequest is what the application asks of a new session.
type SessionRequest struct {
	// Key or Prefix (exactly one) is where uploads may go; it is cleaned
	// and traversal-checked like every key.
	Key    string
	Prefix string
	// MaxSize, when positive, caps each file below the policy's MaxSize
	// (and not below its MinSize).
	MaxSize int64
	// Allowed, when set, narrows the policy's types; each pattern must lie
	// within the policy's Allowed ("image/png" within "image/*").
	Allowed []string
	// TTL is how long the session lasts: required, at most
	// MaxSessionLifetime.
	TTL time.Duration
}

// IssuedSession is a new session with its token, which is shown only
// here: hand it to the client, which sends it in HeaderUploadSession.
type IssuedSession struct {
	Session
	Token string
}

// SessionStore keeps upload sessions. It is the seam for sharing them
// across instances; MemorySessions is the in-process one and
// records/uploadsessions the database one. Implementations store and
// return sessions as given; the Sessions type does every check.
type SessionStore interface {
	// SaveSession records a new session; an ID already present is
	// CodeExists.
	SaveSession(ctx context.Context, s Session) error
	// LoadSession returns the session with id, expired or not;
	// CodeNotFound when there is none.
	LoadSession(ctx context.Context, id string) (Session, error)
	// DeleteSession removes the session with id (revocation); an unknown
	// id is not an error.
	DeleteSession(ctx context.Context, id string) error
	// SweepSessions deletes every session expired at now and returns how
	// many it deleted.
	SweepSessions(ctx context.Context, now time.Time) (int, error)
}

// SessionsOptions configure NewSessions.
type SessionsOptions struct {
	// Now is the clock every expiry is judged by; nil means time.Now.
	Now func() time.Time
}

// Sessions issues and redeems upload sessions over a SessionStore. Create
// it with NewSessions; it is safe for concurrent use.
type Sessions struct {
	store SessionStore
	now   func() time.Time
}

// NewSessions returns Sessions over store. A nil store is
// CodeInvalidArgument.
func NewSessions(store SessionStore, opts SessionsOptions) (*Sessions, error) {
	if store == nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "upload sessions need a store")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Sessions{store: store, now: opts.Now}, nil
}

// errSessionRefused is the one error a client sees for any unusable token
// (missing, malformed, unknown, revoked, expired, wrong secret), so
// tokens cannot be probed: CodePermission, answered 403.
func errSessionRefused() error {
	return filesystem.Errorf(filesystem.CodePermission, "upload session is missing, unknown or expired")
}

// Issue records a new session for uploads under base, the policy of the
// endpoint the token will be redeemed at, and returns it with its token.
// The request may only narrow base: a MaxSize above base's (or below its
// MinSize), a type outside base's Allowed, no or both of Key and Prefix,
// an unclean key or a TTL outside (0, MaxSessionLifetime] is
// CodeInvalidArgument, and an invalid base fails as Validate does.
func (m *Sessions) Issue(ctx context.Context, base Policy, req SessionRequest) (IssuedSession, error) {
	if err := base.Validate(); err != nil {
		return IssuedSession{}, err
	}
	s, err := sessionOf(base, req)
	if err != nil {
		return IssuedSession{}, err
	}
	secret := make([]byte, sessionSecretBytes)
	_, _ = rand.Read(secret) // crypto/rand.Read never fails (it aborts the process instead)
	encoded := base64.RawURLEncoding.EncodeToString(secret)
	now := m.now()
	s.ID = SessionIDPrefix + strings.ToLower(rand.Text())
	s.SecretHash = secretHash(encoded)
	s.CreatedAt, s.ExpiresAt = now, now.Add(req.TTL)
	if err := m.store.SaveSession(ctx, s); err != nil {
		return IssuedSession{}, err
	}
	return IssuedSession{Session: s, Token: s.ID + sessionTokenSeparator + encoded}, nil
}

// sessionOf checks req against base and returns the session it asks for,
// without ID, secret or times.
func sessionOf(base Policy, req SessionRequest) (Session, error) {
	if req.TTL <= 0 || req.TTL > MaxSessionLifetime {
		return Session{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "upload session TTL %s is not in (0, %s]", req.TTL, MaxSessionLifetime)
	}
	if (req.Key == "") == (req.Prefix == "") {
		return Session{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "upload session needs exactly one of Key and Prefix")
	}
	s := Session{MaxSize: req.MaxSize}
	var err error
	if req.Key != "" {
		s.Key, err = filesystem.CleanKey(req.Key)
	} else {
		s.Prefix, err = filesystem.CleanKey(req.Prefix)
	}
	if err != nil {
		return Session{}, err
	}
	if req.MaxSize < 0 || req.MaxSize > base.MaxSize || (req.MaxSize > 0 && req.MaxSize < base.MinSize) {
		return Session{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "upload session MaxSize %d is not in [MinSize, MaxSize] of the policy", req.MaxSize)
	}
	if len(req.Allowed) > MaxSessionTypes {
		return Session{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "upload session allows more than %d types", MaxSessionTypes)
	}
	for _, raw := range req.Allowed {
		pattern := strings.ToLower(strings.TrimSpace(raw))
		if !validTypePattern(pattern) || !withinPatterns(pattern, base.Allowed) {
			return Session{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "upload session type %q is not a type pattern the policy allows", raw)
		}
		s.Allowed = append(s.Allowed, pattern)
	}
	return s, nil
}

// validTypePattern accepts "type/subtype", "type/*" and "*/*": two
// non-empty halves of printable ASCII without spaces or parameters, a
// "*" type only as "*/*".
func validTypePattern(p string) bool {
	typ, sub, ok := strings.Cut(p, "/")
	if !ok || typ == "" || sub == "" || len(p) > maxTypePatternBytes || strings.Contains(sub, "/") || (typ == "*" && sub != "*") {
		return false
	}
	for _, r := range p {
		if r <= ' ' || r > '~' || r == ';' || r == ',' {
			return false
		}
	}
	return true
}

// withinPatterns reports whether every type pattern matches lies within
// one of patterns: an exact type within any pattern matching it, "x/*"
// only within "x/*" or "*/*", "*/*" only within "*/*". It is what keeps a
// session from widening its policy.
func withinPatterns(pattern string, patterns []string) bool {
	const anyType = "*/*"
	if pattern == anyType {
		return slices.ContainsFunc(patterns, func(p string) bool { return strings.EqualFold(p, anyType) })
	}
	if strings.HasSuffix(pattern, "/*") {
		return slices.ContainsFunc(patterns, func(p string) bool { return strings.EqualFold(p, pattern) || strings.EqualFold(p, anyType) })
	}
	return sniff.MatchesAny(pattern, patterns)
}

// secretHash is the hex SHA-256 of a token's secret half.
func secretHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Redeem checks token and returns base narrowed to its session (size,
// types, key or prefix) with the session. Any unusable token is the same
// CodePermission error. The session's types are re-checked against base
// as it is now: a type base no longer allows is dropped, and a session
// left with none is refused (CodeRejected), so changing the endpoint's
// policy narrows sessions already issued. Receive, Confirm and every
// handler then enforce the returned policy on the bytes.
func (m *Sessions) Redeem(ctx context.Context, token string, base Policy) (Policy, Session, error) {
	s, err := m.lookup(ctx, token)
	if err != nil {
		return Policy{}, Session{}, err
	}
	p, err := narrow(base, s)
	if err != nil {
		return Policy{}, Session{}, err
	}
	return p, s, nil
}

// lookup returns the live session token names, or errSessionRefused.
func (m *Sessions) lookup(ctx context.Context, token string) (Session, error) {
	id, secret, ok := strings.Cut(token, sessionTokenSeparator)
	if !ok || len(token) > maxSessionTokenBytes || !strings.HasPrefix(id, SessionIDPrefix) || secret == "" {
		return Session{}, errSessionRefused()
	}
	s, err := m.store.LoadSession(ctx, id)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return Session{}, errSessionRefused()
	}
	if err != nil {
		return Session{}, err
	}
	// The store's word on the ID is not enough: the secret is compared in
	// constant time, and the expiry judged by this clock, whatever the
	// store's sweep has or has not done.
	if subtle.ConstantTimeCompare([]byte(secretHash(secret)), []byte(s.SecretHash)) != 1 || s.ID != id || s.Expired(m.now()) {
		return Session{}, errSessionRefused()
	}
	return s, nil
}

// narrow is base limited to session s.
func narrow(base Policy, s Session) (Policy, error) {
	p := base
	if s.MaxSize > 0 && s.MaxSize < p.MaxSize {
		p.MaxSize = s.MaxSize
	}
	if len(s.Allowed) > 0 {
		var allowed []string
		for _, pattern := range s.Allowed {
			if withinPatterns(pattern, base.Allowed) {
				allowed = append(allowed, pattern)
			}
		}
		if len(allowed) == 0 {
			return Policy{}, filesystem.Errorf(filesystem.CodeRejected, "the upload session's types are no longer allowed")
		}
		p.Allowed = allowed
	}
	switch {
	case s.Key != "":
		key := s.Key
		p.Key = func(Info) (string, error) { return key, nil }
	case base.Key == nil:
		p.Key = RandomKey(s.Prefix)
	default:
		inner, prefix := base.Key, s.Prefix
		p.Key = func(info Info) (string, error) {
			k, err := inner(info)
			if err != nil {
				return "", err
			}
			return filesystem.JoinKey(prefix, k), nil
		}
	}
	return p, nil
}

// Resolver returns an upload Resolver that redeems the token in each
// request's HeaderUploadSession, for the Resolve option of Handler,
// Chunked, Uppy, PresignHandlerWith and LimitsHandler: a request without
// a usable session is refused before its body is read (403), and one with
// a session runs under the session's narrowed policy. Every request of a
// chunked or multipart upload carries the token, so a session that
// expires mid-upload stops it at the next request.
func (m *Sessions) Resolver() Resolver {
	return func(r *http.Request, base Policy) (Policy, error) {
		p, _, err := m.Redeem(r.Context(), r.Header.Get(HeaderUploadSession), base)
		return p, err
	}
}

// Revoke deletes the session with id, so its token is refused from now
// on. An unknown id is not an error.
func (m *Sessions) Revoke(ctx context.Context, id string) error {
	return m.store.DeleteSession(ctx, id)
}

// Sweep deletes the sessions expired by the Sessions clock and returns
// how many. Schedule it; the library starts no background work. An
// expired session is refused whether or not it was swept.
func (m *Sessions) Sweep(ctx context.Context) (int, error) {
	return m.store.SweepSessions(ctx, m.now())
}

// MemorySessions is a SessionStore in this process's memory: sessions do
// not survive a restart and are not shared between instances (behind a
// load balancer, use a shared store such as records/uploadsessions). The
// zero value is ready to use.
type MemorySessions struct {
	mu       sync.Mutex
	sessions map[string]Session
}

// SaveSession implements SessionStore.
func (m *MemorySessions) SaveSession(_ context.Context, s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[s.ID]; ok {
		return filesystem.Errorf(filesystem.CodeExists, "upload session %q exists", s.ID)
	}
	if m.sessions == nil {
		m.sessions = map[string]Session{}
	}
	s.Allowed = slices.Clone(s.Allowed)
	m.sessions[s.ID] = s
	return nil
}

// LoadSession implements SessionStore.
func (m *MemorySessions) LoadSession(_ context.Context, id string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return Session{}, filesystem.Errorf(filesystem.CodeNotFound, "upload session %q not found", id)
	}
	s.Allowed = slices.Clone(s.Allowed)
	return s, nil
}

// DeleteSession implements SessionStore.
func (m *MemorySessions) DeleteSession(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	return nil
}

// SweepSessions implements SessionStore.
func (m *MemorySessions) SweepSessions(_ context.Context, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, s := range m.sessions {
		if s.Expired(now) {
			delete(m.sessions, id)
			n++
		}
	}
	return n, nil
}
