// Package webhooks is a records plugin that delivers chosen events (file
// changes, plugin events such as quota.usage_changed or
// egress.daily_total) to HTTPS endpoints registered per scope (or for
// every scope, Register with Global), signed with HMAC-SHA256 in the
// Standard Webhooks format (or by a Signer of the application's), retried
// with exponential backoff, and refused for any address outside the
// public internet. Secrets rotate without downtime: an endpoint holds up
// to MaxSecrets at once and every delivery is signed with each.
//
// Delivery is an outbox. OnEvent writes one delivery row per matching
// endpoint inside the change's own transaction, so a change that rolls
// back sends nothing and one that commits is never lost. Deliver, run on
// a schedule (jobs.WebhookDeliver), posts the due rows; a failure moves a
// row's next attempt out by Backoff until MaxAttempts, after which it is
// marked failed. Every attempt carries the event's ID (records.Event.ID)
// as Webhook-Id, so a receiver that keeps the IDs it handled drops the
// repeats that at-least-once delivery brings.
package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// PluginName is the plugin's name in the migration ledger.
const PluginName = "webhooks"

// Table names of the plugin.
const (
	TableNameEndpoints  = "fs_webhook_endpoints"
	TableNameDeliveries = "fs_webhook_deliveries"
)

// Column names of fs_webhook_endpoints.
const (
	ColEndpointID        = "id"
	ColEndpointScope     = "scope"
	ColEndpointURL       = "url"
	ColEndpointSecret    = "secret"
	ColEndpointKinds     = "kinds"
	ColEndpointCreatedAt = "created_at"
	// ColEndpointGlobal is 1 for an endpoint that takes events of every
	// scope (Register with Global; its scope column holds ""), 0 for one
	// scope's (migration 0002_webhooks_global).
	ColEndpointGlobal = "is_global"
)

// Values of ColEndpointGlobal.
const (
	endpointScoped int64 = 0
	endpointGlobal int64 = 1
)

// Table and typed columns of fs_webhook_endpoints. Kinds holds the event
// kinds an endpoint wants, separated by kindSeparator, "" for every kind.
// Secret holds the endpoint's active secrets, newest first, separated by
// secretSeparator (one secret for an endpoint never rotated), sealed as a
// whole with WithSealer.
var (
	EndpointTable     = sqlb.Table{Name: TableNameEndpoints, Alias: "whe"}
	EndpointID        = sqlb.Col[string](EndpointTable, ColEndpointID)
	EndpointScope     = sqlb.Col[string](EndpointTable, ColEndpointScope)
	EndpointURL       = sqlb.Col[string](EndpointTable, ColEndpointURL)
	EndpointSecret    = sqlb.Col[string](EndpointTable, ColEndpointSecret)
	EndpointKinds     = sqlb.Col[string](EndpointTable, ColEndpointKinds)
	EndpointCreatedAt = sqlb.Col[time.Time](EndpointTable, ColEndpointCreatedAt)
	EndpointGlobal    = sqlb.Col[int64](EndpointTable, ColEndpointGlobal)
)

// Column names of fs_webhook_deliveries.
const (
	ColDeliveryID         = "id"
	ColDeliveryEndpointID = "endpoint_id"
	ColDeliveryScope      = "scope"
	ColDeliveryEventID    = "event_id"
	ColDeliveryKind       = "kind"
	ColDeliveryPayload    = "payload"
	ColDeliveryState      = "state"
	ColDeliveryAttempts   = "attempts"
	ColDeliveryNextAt     = "next_at"
	ColDeliveryLastError  = "last_error"
	ColDeliveryCreatedAt  = "created_at"
	ColDeliveryUpdatedAt  = "updated_at"
	// ColDeliveryHeaders holds the request headers WithEndpointPayload
	// chose (migration 0003_webhooks_headers): "" for the default, a JSON
	// body sent as application/json; otherwise the JSON of an http.Header
	// holding Content-Type and any extra headers.
	ColDeliveryHeaders = "headers"
)

// Table and typed columns of fs_webhook_deliveries.
var (
	DeliveryTable      = sqlb.Table{Name: TableNameDeliveries, Alias: "whd"}
	DeliveryID         = sqlb.Col[string](DeliveryTable, ColDeliveryID)
	DeliveryEndpointID = sqlb.Col[string](DeliveryTable, ColDeliveryEndpointID)
	DeliveryScope      = sqlb.Col[string](DeliveryTable, ColDeliveryScope)
	DeliveryEventID    = sqlb.Col[string](DeliveryTable, ColDeliveryEventID)
	DeliveryKind       = sqlb.Col[string](DeliveryTable, ColDeliveryKind)
	DeliveryPayload    = sqlb.Col[string](DeliveryTable, ColDeliveryPayload)
	DeliveryState      = sqlb.Col[string](DeliveryTable, ColDeliveryState)
	DeliveryAttempts   = sqlb.Col[int64](DeliveryTable, ColDeliveryAttempts)
	DeliveryNextAt     = sqlb.Col[time.Time](DeliveryTable, ColDeliveryNextAt)
	DeliveryLastError  = sqlb.Col[string](DeliveryTable, ColDeliveryLastError)
	DeliveryCreatedAt  = sqlb.Col[time.Time](DeliveryTable, ColDeliveryCreatedAt)
	DeliveryUpdatedAt  = sqlb.Col[time.Time](DeliveryTable, ColDeliveryUpdatedAt)
	DeliveryHeaders    = sqlb.Col[string](DeliveryTable, ColDeliveryHeaders)
)

// State is where a delivery stands.
type State string

// The closed set of delivery states.
const (
	StatePending   State = "pending"
	StateDelivered State = "delivered"
	// StateFailed is a delivery that used up MaxAttempts, or whose
	// endpoint was removed; it is never tried again.
	StateFailed State = "failed"
)

// ID prefixes.
const (
	EndpointIDPrefix = "whe_"
	DeliveryIDPrefix = "whd_"
)

// Defaults and limits.
const (
	// DefaultMaxAttempts is how many times a delivery is tried before it
	// is marked failed, when WithMaxAttempts is not given: with
	// DefaultBackoff that spans about a day.
	DefaultMaxAttempts = 10
	// DefaultTimeout bounds one attempt with the default client.
	DefaultTimeout = 10 * time.Second
	// DefaultBatch is how many due deliveries one Deliver run takes,
	// unless WithBatch says otherwise.
	DefaultBatch = 100
	// DefaultConcurrency is how many deliveries one Deliver run sends at
	// once, unless WithConcurrency says otherwise: one after another.
	DefaultConcurrency = 1
	// backoffBase and backoffCap shape DefaultBackoff.
	backoffBase = 30 * time.Second
	backoffCap  = 6 * time.Hour
	// claimLease is how long a claimed delivery is hidden from other
	// Deliver runs while it is attempted, from the attempt's own start;
	// past it, a crashed run's claim expires and the delivery is tried
	// again.
	claimLease = 5 * time.Minute
	// attemptTimeout bounds one attempt whatever the client, well inside
	// claimLease, so a live attempt never outlasts its claim.
	attemptTimeout = claimLease / 2
	// kindSeparator joins an endpoint's kinds in its column.
	kindSeparator = " "
	// maxErrorBytes caps a stored last_error (SQLTypes.Text).
	maxErrorBytes = 255
	// maxURLBytes is the longest endpoint URL (SQLTypes.Long).
	maxURLBytes = 1024
	// secretBytes is the random length of a generated secret, and the
	// least a caller's secret may decode to: Standard Webhooks asks for
	// 24 to 64 bytes, and a shorter key would lower the floor generated
	// secrets set.
	secretBytes = 24
	// maxSecretBytes is the most a caller's secret may decode to.
	maxSecretBytes = 64
	// MaxSecrets is how many secrets an endpoint holds at once while it
	// rotates: each adds a signature of about 50 bytes to every delivery's
	// headers, and more than a few means old ones were never retired.
	MaxSecrets = 5
	// secretSeparator joins an endpoint's secrets in its column; no
	// "whsec_" secret contains a space.
	secretSeparator = " "
)

// DefaultBackoff waits 30 seconds after the first failure and doubles
// each time, up to six hours.
func DefaultBackoff(attempt int) time.Duration {
	d := backoffBase
	for i := 1; i < attempt && d < backoffCap; i++ {
		d *= 2
	}
	return min(d, backoffCap)
}

// Plugin delivers events. Construct it with New and install it with
// records.WithPlugins.
type Plugin struct {
	client      *http.Client
	guarded     bool
	backoff     func(attempt int) time.Duration
	maxAttempts int
	logger      *slog.Logger
	sealer      Sealer
	// signer is WithSigner's; StandardSigner by default.
	signer Signer
	// payload is WithPayload's; nil sends DefaultPayload.
	payload PayloadFunc
	// endpointPayload is WithEndpointPayload's; when set it builds every
	// delivery's body and payload is not used.
	endpointPayload EndpointPayloadFunc
	// batch and concurrency are WithBatch's and WithConcurrency's.
	batch, concurrency int
}

// Sealer protects endpoint secrets at rest: Seal turns a secret into the
// text stored in fs_webhook_endpoints, Open turns that text back. Back it
// with a KMS, or an AEAD whose key lives outside the database, so a dump
// of the table does not hand out the means to forge deliveries. Without
// one (the default) secrets are stored as they are, since the plugin must
// read them back to sign.
type Sealer interface {
	Seal(ctx context.Context, secret string) (string, error)
	Open(ctx context.Context, sealed string) (string, error)
}

// WithSealer stores every new endpoint's secret sealed by sl and opens it
// to sign. Endpoints added before it was set hold plaintext, which Open
// receives as is: remove and re-add them, or seal them in a migration of
// your own.
func WithSealer(sl Sealer) Option { return func(p *Plugin) { p.sealer = sl } }

// Option configures a Plugin.
type Option func(*Plugin)

// WithClient replaces the guarded default client: the escape hatch for a
// proxy, mutual TLS, or tests against a local server. The application's
// client must then do its own guarding against internal addresses, and
// AddEndpoint no longer refuses literal internal addresses either.
func WithClient(c *http.Client) Option { return func(p *Plugin) { p.client, p.guarded = c, false } }

// WithBackoff sets the wait after a delivery's n-th failed attempt
// (DefaultBackoff otherwise).
func WithBackoff(fn func(attempt int) time.Duration) Option {
	return func(p *Plugin) { p.backoff = fn }
}

// WithMaxAttempts sets how many attempts a delivery gets before it is
// marked failed (DefaultMaxAttempts otherwise).
func WithMaxAttempts(n int) Option { return func(p *Plugin) { p.maxAttempts = n } }

// WithLogger logs every failed attempt (slog.Default otherwise).
func WithLogger(l *slog.Logger) Option { return func(p *Plugin) { p.logger = l } }

// WithSigner signs every delivery with sg instead of StandardSigner
// (Standard Webhooks v1, HMAC-SHA256 under each active secret): for
// receivers that check ed25519 (Standard Webhooks v1a), a GitHub-style
// X-Hub-Signature-256, or a key held in a KMS. A Signer that fails fails
// the attempt, which is retried like any other: nothing is ever sent
// unsigned.
func WithSigner(sg Signer) Option { return func(p *Plugin) { p.signer = sg } }

// PayloadFunc builds the body of the deliveries of one event: any value
// encoding/json marshals (a CloudEvents envelope, a trimmed body, the
// application's own schema). DefaultPayload is the body without it, to
// wrap or extend.
type PayloadFunc func(ctx context.Context, e records.Event) (any, error)

// WithPayload builds every delivery's JSON body with fn instead of
// DefaultPayload. It runs once per event inside the change's transaction,
// when at least one endpoint wants the event; an error from fn, or a value
// that does not marshal, fails the change (it rolls back), so no event is
// dropped or sent in a shape the receivers were not promised.
func WithPayload(fn PayloadFunc) Option { return func(p *Plugin) { p.payload = fn } }

// DefaultContentType is a delivery's Content-Type unless
// WithEndpointPayload names another.
const DefaultContentType = "application/json"

// Body is one delivery's request body as WithEndpointPayload builds it.
type Body struct {
	// Data is sent as it is. It is stored as text until delivered, so it
	// must be valid UTF-8 (JSON, XML, a form): a binary body is refused.
	Data []byte
	// ContentType is the Content-Type header; "" is DefaultContentType.
	// "application/cloudevents+json" is CloudEvents' structured mode.
	ContentType string
	// Header holds extra request headers, such as the ce-* headers of
	// CloudEvents' binary mode. The signing headers (Webhook-*), Host and
	// the framing headers (Content-Length, Transfer-Encoding, Connection)
	// are refused: the signer and the transport own them. The Signer runs
	// after these are set, so a header it writes always wins.
	Header http.Header
}

// EndpointPayloadFunc builds the body of one event's delivery to one
// endpoint, so receivers of one event can each get their own shape: a
// Slack message for one, a CloudEvents envelope for another, a non-JSON
// body for a third. ep carries the endpoint's URL, scope and kinds.
type EndpointPayloadFunc func(ctx context.Context, e records.Event, ep Endpoint) (Body, error)

// WithEndpointPayload builds every delivery's body with fn, once per
// endpoint that wants the event, instead of WithPayload's single JSON body
// for all of them; WithPayload is then not used. It runs inside the
// change's transaction: an error from fn, a body that is not valid UTF-8
// or a refused header fails the change (it rolls back), so no event is
// dropped or sent in a shape the receiver was not promised. The body and
// headers are stored with the delivery, so a retry sends the same bytes.
// nil keeps WithPayload's, or DefaultPayload.
func WithEndpointPayload(fn EndpointPayloadFunc) Option {
	return func(p *Plugin) { p.endpointPayload = fn }
}

// reservedHeader reports whether a Body may not set name.
func reservedHeader(name string) bool {
	switch k := http.CanonicalHeaderKey(name); {
	case strings.HasPrefix(k, "Webhook-"), k == "Host", k == "Content-Length", k == "Transfer-Encoding", k == "Connection":
		return true
	}
	return false
}

// encode is b's stored form: its data, and its headers as the
// ColDeliveryHeaders text ("" for a default JSON body without extras).
func (b Body) encode() (data, headers string, err error) {
	if !utf8.Valid(b.Data) {
		return "", "", filesystem.Errorf(filesystem.CodeInvalidArgument, "webhooks: a delivery body must be valid UTF-8")
	}
	if (b.ContentType == "" || b.ContentType == DefaultContentType) && len(b.Header) == 0 {
		return string(b.Data), "", nil
	}
	h := http.Header{}
	for k, vs := range b.Header {
		if reservedHeader(k) {
			return "", "", filesystem.Errorf(filesystem.CodeInvalidArgument, "webhooks: a delivery body may not set the %s header", http.CanonicalHeaderKey(k))
		}
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	ct := b.ContentType
	if ct == "" {
		ct = DefaultContentType
	}
	h.Set("Content-Type", ct)
	enc, err := json.Marshal(h)
	return string(b.Data), string(enc), err
}

// WithBatch sets how many due deliveries one Deliver run takes
// (DefaultBatch otherwise), at least 1: raise it, with WithConcurrency, for
// high-volume delivery, or schedule Deliver more often.
func WithBatch(n int) Option { return func(p *Plugin) { p.batch = n } }

// WithConcurrency sets how many deliveries one Deliver run sends at once
// (DefaultConcurrency, one after another, otherwise), at least 1. Each
// delivery is still claimed on its own before it is sent, so concurrent
// sends, and runs on several machines, never send one delivery twice at
// the same time. The Store's database must allow that many concurrent
// statements.
func WithConcurrency(n int) Option { return func(p *Plugin) { p.concurrency = n } }

// New returns a Plugin. A nil client, backoff, logger or signer, fewer
// than one attempt, or a batch or concurrency below 1, is
// CodeInvalidArgument.
func New(opts ...Option) (*Plugin, error) {
	p := &Plugin{
		client: guardedClient(), guarded: true, backoff: DefaultBackoff, maxAttempts: DefaultMaxAttempts, logger: slog.Default(),
		signer: StandardSigner{}, batch: DefaultBatch, concurrency: DefaultConcurrency,
	}
	for _, o := range opts {
		o(p)
	}
	if p.client == nil || p.backoff == nil || p.logger == nil || p.signer == nil || p.maxAttempts < 1 || p.batch < 1 || p.concurrency < 1 {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "webhooks: a nil client, backoff, logger or signer, or fewer than one attempt, batch or concurrent send")
	}
	return p, nil
}

// Name implements records.Plugin.
func (p *Plugin) Name() string { return PluginName }

var _ records.EventHooks = (*Plugin)(nil)

// Migrations implements records.Migrator.
func (p *Plugin) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	st, err := records.NewDDL(engine).CreateTable(records.Table{
		Table: EndpointTable,
		Columns: []ddl.ColumnSpec{
			records.Column(EndpointID, t.Key), records.Column(EndpointScope, t.Key), records.Column(EndpointURL, t.Long),
			records.Column(EndpointSecret, t.Doc), records.Column(EndpointKinds, t.Doc), records.Column(EndpointCreatedAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{EndpointID},
		Indexes:    []records.Index{{Name: "fs_webhook_endpoints_scope", Columns: []records.IndexColumn{records.Key(EndpointScope)}}},
	}).CreateTable(records.Table{
		Table: DeliveryTable,
		Columns: []ddl.ColumnSpec{
			records.Column(DeliveryID, t.Key), records.Column(DeliveryEndpointID, t.Key), records.Column(DeliveryScope, t.Key),
			records.Column(DeliveryEventID, t.Key), records.Column(DeliveryKind, t.Short), records.Column(DeliveryPayload, t.Doc),
			records.Column(DeliveryState, t.Short), records.Column(DeliveryAttempts, t.Int), records.Column(DeliveryNextAt, t.Timestamp),
			records.Column(DeliveryLastError, t.Text), records.Column(DeliveryCreatedAt, t.Timestamp), records.Column(DeliveryUpdatedAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{DeliveryID},
		Indexes: []records.Index{
			{Name: "fs_webhook_deliveries_due", Columns: []records.IndexColumn{records.Key(DeliveryState), records.Key(DeliveryNextAt)}},
			{Name: "fs_webhook_deliveries_endpoint", Columns: []records.IndexColumn{records.Key(DeliveryEndpointID), records.Key(DeliveryCreatedAt)}},
		},
	}).Statements()
	global, err2 := records.NewDDL(engine).AddColumn(EndpointTable, records.Column(EndpointGlobal, t.Int).Default(endpointScoped)).Statements()
	headers, err3 := headersColumn(engine, t).Statements()
	return []records.Migration{
		{Owner: PluginName, Name: "0001_webhooks", Statements: st},
		{Owner: PluginName, Name: "0002_webhooks_global", Statements: global},
		{Owner: PluginName, Name: "0003_webhooks_headers", Statements: headers},
	}, errors.Join(err, err2, err3)
}

// headersColumn adds ColDeliveryHeaders, "" on every existing delivery.
// MySQL gives a TEXT column no literal default, so there it is added
// nullable, back-filled with "" and then made NOT NULL. Every statement
// can run again after a crash before the ledger write (records.DDL).
func headersColumn(engine string, t records.SQLTypes) *records.DDL {
	if engine != records.EngineMySQL {
		return records.NewDDL(engine).AddColumn(DeliveryTable, records.Column(DeliveryHeaders, t.Doc).Default(""))
	}
	return records.NewDDL(engine).AddColumn(DeliveryTable, records.Column(DeliveryHeaders, t.Doc).Nullable()).
		// A data backfill, not DDL: written by hand, and re-runnable.
		Raw("UPDATE "+TableNameDeliveries+" SET "+ColDeliveryHeaders+" = '' WHERE "+ColDeliveryHeaders+" IS NULL").
		ModifyColumn(DeliveryTable, records.Column(DeliveryHeaders, t.Doc))
}

// Secret is an endpoint's signing secret, "whsec_" and base64 as Standard
// Webhooks writes it. It prints, logs and marshals as "[redacted]", so it
// never leaks through fmt, slog or JSON; Reveal returns the value, to show
// once to whoever registers the endpoint.
type Secret string

const redacted = "[redacted]"

// String implements fmt.Stringer.
func (Secret) String() string { return redacted }

// GoString implements fmt.GoStringer, for %#v.
func (Secret) GoString() string { return redacted }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalText implements encoding.TextMarshaler, which JSON uses.
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// Reveal returns the secret itself.
func (s Secret) Reveal() string { return string(s) }

// Endpoint is a URL one scope's events, or every scope's, are delivered
// to.
type Endpoint struct {
	ID string
	// Scope is the scope whose events are delivered; "" for a Global
	// endpoint.
	Scope string
	URL   string
	// Kinds lists the event kinds delivered; empty delivers every kind.
	Kinds []records.EventKind
	// Secret is the newest active secret, Secrets[0]: the one a receiver
	// set up now should be given.
	Secret Secret
	// Secrets are every active secret, newest first; each signs every
	// delivery while the endpoint rotates (RotateSecret). One secret for
	// an endpoint never rotated.
	Secrets []Secret
	// Global is an endpoint that takes the events of every scope.
	Global    bool
	CreatedAt time.Time
}

// EndpointSpec is what Register registers. It is a struct so fields can
// be added without breaking callers.
type EndpointSpec struct {
	// Scope is the scope whose events are delivered. Ignored (stored as
	// "") when Global is set.
	Scope string
	// URL must be http or https with a host and no credentials, at most
	// 1024 bytes.
	URL string
	// Kinds lists the event kinds delivered; empty delivers every kind.
	Kinds []records.EventKind
	// Secret is the signing secret, "whsec_" and the base64 of 24 to 64
	// bytes, for a receiver that already has one (a migration from another
	// sender); empty generates a random one.
	Secret Secret
	// Global takes the events of every scope: one internal consumer (a
	// search indexer, a data warehouse) for all tenants. Never hand a
	// global endpoint to a tenant: it sees every tenant's events.
	Global bool
}

// AddEndpoint registers url for scope's events of kinds (every kind when
// empty) and returns it with a new random secret, which the receiver
// needs to check signatures (Endpoint.Secret.Reveal). The URL must be
// http or https with a host and no credentials; with the default client a
// host that is a literal internal address is refused here, and every
// address a name resolves to is checked again on each attempt. Register
// takes a secret of the caller's, or registers a global endpoint.
func (p *Plugin) AddEndpoint(ctx context.Context, s *records.Store, scope, rawURL string, kinds ...records.EventKind) (Endpoint, error) {
	return p.Register(ctx, s, EndpointSpec{Scope: scope, URL: rawURL, Kinds: kinds})
}

// AddGlobalEndpoint registers url for the events of kinds (every kind when
// empty) of every scope, with a new random secret: Register with Global.
func (p *Plugin) AddGlobalEndpoint(ctx context.Context, s *records.Store, rawURL string, kinds ...records.EventKind) (Endpoint, error) {
	return p.Register(ctx, s, EndpointSpec{URL: rawURL, Kinds: kinds, Global: true})
}

// Register registers the endpoint spec describes and returns it, its
// secret filled (the caller's, or a new random one). The URL is checked
// as AddEndpoint does; a caller's secret that is not "whsec_" and the
// base64 of 24 to 64 bytes is CodeInvalidArgument, unless the Signer
// implements SecretFormat, whose check then applies with the
// MinSecretBytes floor.
func (p *Plugin) Register(ctx context.Context, s *records.Store, spec EndpointSpec) (Endpoint, error) {
	if err := p.checkURL(spec.URL); err != nil {
		return Endpoint{}, err
	}
	names := make([]string, len(spec.Kinds))
	for i, k := range spec.Kinds {
		if k == "" || strings.Contains(string(k), kindSeparator) || len(k) > records.MaxEventKindBytes {
			return Endpoint{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "webhooks: event kind %q is not valid", k)
		}
		names[i] = string(k)
	}
	secret, err := p.secretOrNew(spec.Secret)
	if err != nil {
		return Endpoint{}, err
	}
	ep := Endpoint{
		ID: s.NewID(EndpointIDPrefix), Scope: spec.Scope, URL: spec.URL, Kinds: spec.Kinds,
		Secret: secret, Secrets: []Secret{secret}, Global: spec.Global, CreatedAt: s.Now(),
	}
	global := endpointScoped
	if spec.Global {
		ep.Scope, global = "", endpointGlobal
	}
	stored, err := p.sealSecrets(ctx, ep.Secrets)
	if err != nil {
		return Endpoint{}, err
	}
	_, err = s.Exec(ctx, sqlb.NewInsert(s.Dialect()).Into(EndpointTable).Row(
		sqlb.Set(EndpointID, ep.ID), sqlb.Set(EndpointScope, ep.Scope), sqlb.Set(EndpointURL, spec.URL),
		sqlb.Set(EndpointSecret, stored), sqlb.Set(EndpointKinds, strings.Join(names, kindSeparator)), sqlb.Set(EndpointCreatedAt, ep.CreatedAt),
		sqlb.Set(EndpointGlobal, global),
	))
	if err != nil {
		return Endpoint{}, err
	}
	return ep, nil
}

// checkURL refuses a URL no delivery should go to.
func (p *Plugin) checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > maxURLBytes || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "webhooks: %q is not an http(s) URL with a host and no credentials, at most %d bytes", raw, maxURLBytes)
	}
	if a, err := netip.ParseAddr(strings.Trim(u.Hostname(), "[]")); err == nil && p.guarded && !upload.PublicAddr(a) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "webhooks: %s is not an address on the public internet", a)
	}
	return nil
}

// RemoveEndpoint deletes endpoint id of scope and reports whether it
// existed. Its pending deliveries are marked failed when next attempted.
// A global endpoint is removed with RemoveGlobalEndpoint.
func (p *Plugin) RemoveEndpoint(ctx context.Context, s *records.Store, scope, id string) (bool, error) {
	return p.remove(ctx, s, EndpointRef{Scope: scope, ID: id})
}

// RemoveGlobalEndpoint deletes global endpoint id and reports whether it
// existed.
func (p *Plugin) RemoveGlobalEndpoint(ctx context.Context, s *records.Store, id string) (bool, error) {
	return p.remove(ctx, s, EndpointRef{ID: id, Global: true})
}

func (p *Plugin) remove(ctx context.Context, s *records.Store, ref EndpointRef) (bool, error) {
	n, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(EndpointTable).Where(ref.where()...))
	return n == 1, err
}

// Endpoints lists scope's endpoints, oldest first, without the global
// ones (GlobalEndpoints). Their secrets are filled but redacted in any
// output; Reveal reads them.
func (p *Plugin) Endpoints(ctx context.Context, s *records.Store, scope string) ([]Endpoint, error) {
	return p.endpoints(ctx, s, sqlb.Eq(EndpointScope, scope), sqlb.Eq(EndpointGlobal, endpointScoped))
}

// GlobalEndpoints lists the global endpoints, oldest first.
func (p *Plugin) GlobalEndpoints(ctx context.Context, s *records.Store) ([]Endpoint, error) {
	return p.endpoints(ctx, s, sqlb.Eq(EndpointGlobal, endpointGlobal))
}

func (p *Plugin) endpoints(ctx context.Context, s *records.Store, where ...sqlb.Predicate) ([]Endpoint, error) {
	var ep Endpoint
	var secret, kinds string
	var global int64
	var out []Endpoint
	q := sqlb.NewSelect(s.Dialect()).Columns(
		sqlb.Bind(EndpointID, &ep.ID), sqlb.Bind(EndpointScope, &ep.Scope), sqlb.Bind(EndpointURL, &ep.URL),
		sqlb.Bind(EndpointSecret, &secret), sqlb.Bind(EndpointKinds, &kinds), sqlb.Bind(EndpointCreatedAt, &ep.CreatedAt),
		sqlb.Bind(EndpointGlobal, &global),
	).From(EndpointTable).Where(where...).Order(sqlb.OrderBy(EndpointCreatedAt, sqlb.SortAsc), sqlb.OrderBy(EndpointID, sqlb.SortAsc))
	err := s.Each(ctx, q, func() error {
		e := ep
		secrets, err := p.openSecrets(ctx, secret)
		if err != nil {
			return err
		}
		e.Secret, e.Secrets, e.Global, e.CreatedAt, e.Kinds = secrets[0], secrets, global == endpointGlobal, ep.CreatedAt.UTC(), nil
		for k := range strings.FieldsSeq(kinds) {
			e.Kinds = append(e.Kinds, records.EventKind(k))
		}
		out = append(out, e)
		return nil
	})
	return out, err
}

// wants reports whether ep takes events of kind.
func (ep Endpoint) wants(kind records.EventKind) bool {
	return len(ep.Kinds) == 0 || slices.Contains(ep.Kinds, kind)
}

// Payload is the JSON body of a delivery.
type Payload struct {
	// ID is the event's ID, also sent as Webhook-Id: the same on every
	// attempt, for the receiver to drop repeats.
	ID           string            `json:"id"`
	Kind         string            `json:"kind"`
	Scope        string            `json:"scope"`
	At           time.Time         `json:"at"`
	FileID       string            `json:"file_id,omitempty"`
	AttachmentID string            `json:"attachment_id,omitempty"`
	OwnerType    string            `json:"owner_type,omitempty"`
	OwnerID      string            `json:"owner_id,omitempty"`
	Role         string            `json:"role,omitempty"`
	Subject      string            `json:"subject,omitempty"`
	Detail       map[string]string `json:"detail,omitempty"`
	File         *FilePayload      `json:"file,omitempty"`
}

// FilePayload is the file an event is about, as the change left it. It
// carries no storage key or disk: a receiver asks the application for
// the bytes.
type FilePayload struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	Visibility  string `json:"visibility"`
	Status      string `json:"status"`
}

// DefaultPayload is the body of an event's deliveries without
// WithPayload: the event, and the file it is about without its storage
// key or disk.
func DefaultPayload(e records.Event) Payload {
	pl := Payload{
		ID: e.ID, Kind: string(e.Kind), Scope: e.Scope, At: e.At, FileID: e.FileID, AttachmentID: e.AttachmentID,
		OwnerType: e.Owner.Type, OwnerID: e.Owner.ID, Role: e.Role, Subject: e.Subject, Detail: e.Detail,
	}
	if f := e.File; f != nil {
		pl.File = &FilePayload{ID: f.ID, Filename: f.Filename, ContentType: f.ContentType, Size: f.Size, SHA256: f.SHA256, Visibility: string(f.Visibility), Status: string(f.Status)}
	}
	return pl
}

// OnEvent implements records.EventHooks: inside the change's transaction
// it writes one pending delivery for every endpoint of the event's scope,
// and every global endpoint, that wants its kind, due at once.
func (p *Plugin) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	eps, err := p.endpoints(ctx, s, sqlb.Or(
		sqlb.And(sqlb.Eq(EndpointScope, e.Scope), sqlb.Eq(EndpointGlobal, endpointScoped)),
		sqlb.Eq(EndpointGlobal, endpointGlobal),
	))
	if err != nil {
		return err
	}
	var shared []byte
	for _, ep := range eps {
		if !ep.wants(e.Kind) {
			continue
		}
		var body, headers string
		if p.endpointPayload != nil {
			b, err := p.endpointPayload(ctx, e, ep)
			if err != nil {
				return err
			}
			if body, headers, err = b.encode(); err != nil {
				return err
			}
		} else {
			if shared == nil {
				if shared, err = p.body(ctx, e); err != nil {
					return err
				}
			}
			body = string(shared)
		}
		_, err := s.Exec(ctx, sqlb.NewInsert(s.Dialect()).Into(DeliveryTable).Row(
			sqlb.Set(DeliveryID, s.NewID(DeliveryIDPrefix)), sqlb.Set(DeliveryEndpointID, ep.ID),
			sqlb.Set(DeliveryScope, e.Scope), sqlb.Set(DeliveryEventID, e.ID), sqlb.Set(DeliveryKind, string(e.Kind)),
			sqlb.Set(DeliveryPayload, body), sqlb.Set(DeliveryState, string(StatePending)), sqlb.Set(DeliveryAttempts, int64(0)),
			sqlb.Set(DeliveryNextAt, e.At), sqlb.Set(DeliveryLastError, ""), sqlb.Set(DeliveryCreatedAt, e.At), sqlb.Set(DeliveryUpdatedAt, e.At),
			sqlb.Set(DeliveryHeaders, headers),
		))
		if err != nil {
			return err
		}
	}
	return nil
}

// body is the JSON body of e's deliveries: WithPayload's value, or
// DefaultPayload.
func (p *Plugin) body(ctx context.Context, e records.Event) ([]byte, error) {
	if p.payload == nil {
		return json.Marshal(DefaultPayload(e))
	}
	v, err := p.payload(ctx, e)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// Delivery is one event on its way to one endpoint.
type Delivery struct {
	ID         string
	EndpointID string
	Scope      string
	EventID    string
	Kind       string
	State      State
	Attempts   int64
	NextAt     time.Time
	LastError  string
}

// Deliveries lists scope's deliveries in state, oldest first, for an
// admin screen or a test.
func (p *Plugin) Deliveries(ctx context.Context, s *records.Store, scope string, state State) ([]Delivery, error) {
	var d Delivery
	var st string
	var out []Delivery
	q := sqlb.NewSelect(s.Dialect()).Columns(
		sqlb.Bind(DeliveryID, &d.ID), sqlb.Bind(DeliveryEndpointID, &d.EndpointID), sqlb.Bind(DeliveryScope, &d.Scope),
		sqlb.Bind(DeliveryEventID, &d.EventID), sqlb.Bind(DeliveryKind, &d.Kind), sqlb.Bind(DeliveryState, &st),
		sqlb.Bind(DeliveryAttempts, &d.Attempts), sqlb.Bind(DeliveryNextAt, &d.NextAt), sqlb.Bind(DeliveryLastError, &d.LastError),
	).From(DeliveryTable).Where(sqlb.Eq(DeliveryScope, scope), sqlb.Eq(DeliveryState, string(state))).
		Order(sqlb.OrderBy(DeliveryCreatedAt, sqlb.SortAsc), sqlb.OrderBy(DeliveryID, sqlb.SortAsc))
	err := s.Each(ctx, q, func() error {
		d.State, d.NextAt = State(st), d.NextAt.UTC()
		out = append(out, d)
		return nil
	})
	return out, err
}

// due is a delivery Deliver claimed, with what it needs to send it.
type due struct {
	id, endpointID, eventID, payload, headers string
	attempts                                  int64
	nextAt                                    time.Time
}

// Deliver attempts up to DefaultBatch (WithBatch) deliveries due at now,
// DefaultConcurrency (WithConcurrency) at a time, and returns how many
// were delivered. Each is first claimed at the moment it is
// attempted (its next attempt moved out by a lease from the Store's clock
// then, if no other run moved it first), so runs on several machines
// never send one delivery at the same time however long a batch takes,
// and a run that crashes mid-attempt leaves it to be retried once the
// lease passes. An attempt is cut off well inside its lease. Its
// Webhook-Timestamp is the Store's clock at the attempt. A
// 2xx answer delivers it; anything else, a redirect included, schedules
// the next attempt after Backoff, or marks it failed after MaxAttempts.
// Errors of single deliveries are recorded on them and logged, not
// returned; the error is for the database (the first one, after every
// send already started has finished).
func (p *Plugin) Deliver(ctx context.Context, s *records.Store, now time.Time) (int, error) {
	now = now.UTC()
	var d due
	var list []due
	q := sqlb.NewSelect(s.Dialect()).Columns(
		sqlb.Bind(DeliveryID, &d.id), sqlb.Bind(DeliveryEndpointID, &d.endpointID), sqlb.Bind(DeliveryEventID, &d.eventID),
		sqlb.Bind(DeliveryPayload, &d.payload), sqlb.Bind(DeliveryAttempts, &d.attempts), sqlb.Bind(DeliveryNextAt, &d.nextAt),
		sqlb.Bind(DeliveryHeaders, &d.headers),
	).From(DeliveryTable).Where(sqlb.Eq(DeliveryState, string(StatePending)), sqlb.LessEq(DeliveryNextAt, now)).
		Order(sqlb.OrderBy(DeliveryNextAt, sqlb.SortAsc)).Limit(p.batch)
	if err := s.Each(ctx, q, func() error { list = append(list, d); return nil }); err != nil {
		return 0, err
	}
	if p.concurrency == 1 {
		delivered := 0
		for _, d := range list {
			ok, err := p.attempt(ctx, s, d)
			if err != nil {
				return delivered, err
			}
			if ok {
				delivered++
			}
		}
		return delivered, nil
	}
	return p.deliverConcurrently(ctx, s, list)
}

// deliverConcurrently attempts list with up to p.concurrency sends at
// once. After the first database error it starts no further attempt, lets
// those under way finish, and returns that error.
func (p *Plugin) deliverConcurrently(ctx context.Context, s *records.Store, list []due) (int, error) {
	var (
		mu        sync.Mutex
		wg        sync.WaitGroup
		delivered int
		first     error
	)
	slots := make(chan struct{}, p.concurrency)
	for _, d := range list {
		mu.Lock()
		stop := first != nil
		mu.Unlock()
		if stop {
			break
		}
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			ok, err := p.attempt(ctx, s, d)
			mu.Lock()
			defer mu.Unlock()
			if ok {
				delivered++
			}
			if err != nil && first == nil {
				first = err
			}
		})
	}
	wg.Wait()
	return delivered, first
}

// attempt claims d, sends it and records the outcome. Every time it uses
// is the Store's clock at the attempt, not the run's start: a slow batch
// must neither sign a stale Webhook-Timestamp nor hold a lease that ran
// out while earlier deliveries were sent. The claim is the attempt count
// (a fencing token): the outcome is written only while it is still ours.
func (p *Plugin) attempt(ctx context.Context, s *records.Store, d due) (bool, error) {
	at := s.Now()
	claimed, err := s.Exec(ctx, sqlb.NewUpdate(s.Dialect()).Table(DeliveryTable).Set(
		sqlb.Set(DeliveryNextAt, at.Add(claimLease)), sqlb.Set(DeliveryAttempts, d.attempts+1), sqlb.Set(DeliveryUpdatedAt, at),
	).Where(sqlb.Eq(DeliveryID, d.id), sqlb.Eq(DeliveryState, string(StatePending)), sqlb.Eq(DeliveryAttempts, d.attempts)))
	if err != nil || claimed == 0 {
		return false, err
	}
	c := claim{id: d.id, attempts: d.attempts + 1}
	eps, err := p.endpoints(ctx, s, sqlb.Eq(EndpointID, d.endpointID))
	if err != nil {
		return false, err
	}
	if len(eps) == 0 {
		return false, p.settle(ctx, s, c, StateFailed, "endpoint removed")
	}
	// The attempt ends well inside its lease, so no other run can claim
	// the delivery while it is still being sent.
	sendCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	sendErr := p.send(sendCtx, eps[0], d.eventID, []byte(d.payload), d.headers, at)
	cancel()
	if sendErr == nil {
		return true, p.settle(ctx, s, c, StateDelivered, "")
	}
	p.logger.WarnContext(ctx, "webhook delivery failed", "delivery", d.id, "endpoint", eps[0].ID, "attempt", c.attempts, "error", sendErr)
	if c.attempts >= int64(p.maxAttempts) {
		return false, p.settle(ctx, s, c, StateFailed, sendErr.Error())
	}
	done := s.Now()
	_, err = s.Exec(ctx, sqlb.NewUpdate(s.Dialect()).Table(DeliveryTable).Set(
		sqlb.Set(DeliveryNextAt, done.Add(p.backoff(int(c.attempts)))), sqlb.Set(DeliveryLastError, clip(sendErr.Error())), sqlb.Set(DeliveryUpdatedAt, done),
	).Where(c.where()...))
	return false, err
}

// claim is a delivery this run holds: its ID and the attempt count the
// claim wrote.
type claim struct {
	id       string
	attempts int64
}

// where selects the delivery only while the claim is still this run's.
func (c claim) where() []sqlb.Predicate {
	return []sqlb.Predicate{sqlb.Eq(DeliveryID, c.id), sqlb.Eq(DeliveryState, string(StatePending)), sqlb.Eq(DeliveryAttempts, c.attempts)}
}

// settle ends a claimed delivery in state.
func (p *Plugin) settle(ctx context.Context, s *records.Store, c claim, state State, why string) error {
	_, err := s.Exec(ctx, sqlb.NewUpdate(s.Dialect()).Table(DeliveryTable).Set(
		sqlb.Set(DeliveryState, string(state)), sqlb.Set(DeliveryLastError, clip(why)), sqlb.Set(DeliveryUpdatedAt, s.Now()),
	).Where(c.where()...))
	return err
}

// clip cuts s to at most maxErrorBytes on a rune boundary, so a stored
// error is always valid UTF-8.
func clip(s string) string {
	if len(s) <= maxErrorBytes {
		return s
	}
	cut := maxErrorBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// errStatus is a non-2xx answer.
var errStatus = errors.New("webhooks: endpoint answered")

// send posts one signed attempt, with the stored headers of a body
// WithEndpointPayload built ("" for a default JSON body).
func (p *Plugin) send(ctx context.Context, ep Endpoint, eventID string, body []byte, headers string, now time.Time) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", DefaultContentType)
	if headers != "" {
		var h http.Header
		if err := json.Unmarshal([]byte(headers), &h); err != nil {
			return err
		}
		for k, vs := range h {
			req.Header[http.CanonicalHeaderKey(k)] = vs
		}
	}
	if err := p.signer.Sign(ctx, SignRequest{Header: req.Header, Endpoint: ep, MessageID: eventID, At: now, Body: body}); err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return errors.Join(errStatus, errors.New(resp.Status))
	}
	return nil
}

// Prune deletes the delivered and failed deliveries last updated before
// cutoff and returns how many it deleted; pending ones are kept whatever
// their age. Schedule it (jobs.WebhookPrune).
func (p *Plugin) Prune(ctx context.Context, s *records.Store, cutoff time.Time) (int64, error) {
	return s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(DeliveryTable).Where(
		sqlb.In(DeliveryState, string(StateDelivered), string(StateFailed)), sqlb.Less(DeliveryUpdatedAt, cutoff.UTC()),
	))
}

// guardedClient is the default client: upload's guarded client (every
// connection only to addresses upload.PublicAddr accepts, checked after
// DNS resolution, no proxy), following no redirect, each request bounded
// by DefaultTimeout.
func guardedClient() *http.Client {
	c := upload.FetchOptions{MaxRedirects: -1}.Client()
	c.Timeout = DefaultTimeout
	return c
}
