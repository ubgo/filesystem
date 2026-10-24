package webhooks

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"unicode"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// EndpointRef names one endpoint: endpoint ID of scope Scope, or, with
// Global, global endpoint ID (Scope is then ignored). Scoped and global
// endpoints are kept apart because "" is a valid scope: a tenant of scope
// "" never reaches a global endpoint through a scoped ref.
type EndpointRef struct {
	Scope  string
	ID     string
	Global bool
}

// where selects the endpoint ref names.
func (ref EndpointRef) where() []sqlb.Predicate {
	if ref.Global {
		return []sqlb.Predicate{sqlb.Eq(EndpointID, ref.ID), sqlb.Eq(EndpointGlobal, endpointGlobal)}
	}
	return []sqlb.Predicate{sqlb.Eq(EndpointScope, ref.Scope), sqlb.Eq(EndpointID, ref.ID), sqlb.Eq(EndpointGlobal, endpointScoped)}
}

// NewSecret returns a random secret: "whsec_" and the base64 of 24 random
// bytes, as Standard Webhooks writes them.
func NewSecret() Secret {
	key := make([]byte, secretBytes)
	_, _ = rand.Read(key) // crypto/rand.Read never fails
	return Secret(secretPrefix + b64.EncodeToString(key))
}

// CheckSecret reports whether secret is one Register and RotateSecret
// accept: "whsec_" and the base64 of 24 to 64 bytes. A shorter key would
// sign with less entropy than a generated secret has.
func CheckSecret(secret Secret) error {
	raw, ok := strings.CutPrefix(secret.Reveal(), secretPrefix)
	key, err := b64.DecodeString(raw)
	if !ok || err != nil || len(key) < secretBytes || len(key) > maxSecretBytes {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "webhooks: a secret is %q and the base64 of %d to %d bytes", secretPrefix, secretBytes, maxSecretBytes)
	}
	return nil
}

// SecretFormat is an optional upgrade of a Signer whose secrets are not
// "whsec_" base64: a GitHub-style X-Hub-Signature-256 signer whose
// receivers already hold a free-form secret, endpoints imported from
// another webhook service, or ed25519 key material. When the plugin's
// Signer implements it, Register and RotateSecret generate secrets with
// NewSecret and check a caller's with CheckSecret instead of the
// package's NewSecret and CheckSecret. The floor stays the plugin's,
// whatever the format: every secret, generated or given, is at least
// MinSecretBytes and at most MaxSecretTextBytes long and holds no
// whitespace, so a lenient CheckSecret cannot let a short secret in.
type SecretFormat interface {
	// NewSecret returns a new random secret in the signer's format.
	NewSecret() (Secret, error)
	// CheckSecret refuses a caller's secret the signer cannot sign with.
	CheckSecret(secret Secret) error
}

// Bounds every secret meets whatever its format (SecretFormat): at least
// as long as the 24 random bytes a generated secret has, and short enough
// for MaxSecrets of them to sit in one column.
const (
	MinSecretBytes     = secretBytes
	MaxSecretTextBytes = 1024
)

// secretOrNew is secret, checked, or a new random one when it is empty,
// in the Signer's format when it has one (SecretFormat), else the
// package's.
func (p *Plugin) secretOrNew(secret Secret) (Secret, error) {
	f, custom := p.signer.(SecretFormat)
	if !custom {
		if secret == "" {
			return NewSecret(), nil
		}
		return secret, CheckSecret(secret)
	}
	if secret == "" {
		var err error
		if secret, err = f.NewSecret(); err != nil {
			return "", err
		}
	} else if err := f.CheckSecret(secret); err != nil {
		return "", err
	}
	if err := checkFloor(secret); err != nil {
		return "", err
	}
	return secret, nil
}

// checkFloor refuses a secret shorter than MinSecretBytes, longer than
// MaxSecretTextBytes, or holding whitespace (secretSeparator joins an
// endpoint's secrets in its column).
func checkFloor(secret Secret) error {
	raw := secret.Reveal()
	if len(raw) < MinSecretBytes || len(raw) > MaxSecretTextBytes || strings.IndexFunc(raw, unicode.IsSpace) >= 0 {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "webhooks: a secret is %d to %d bytes without whitespace", MinSecretBytes, MaxSecretTextBytes)
	}
	return nil
}

// sealSecrets is the stored form of secrets (newest first): joined by
// secretSeparator, then sealed with WithSealer. One secret stores exactly
// as it did before rotation existed, so rows written then still read.
func (p *Plugin) sealSecrets(ctx context.Context, secrets []Secret) (string, error) {
	plain := make([]string, len(secrets))
	for i, sec := range secrets {
		plain[i] = sec.Reveal()
	}
	joined := strings.Join(plain, secretSeparator)
	if p.sealer == nil {
		return joined, nil
	}
	return p.sealer.Seal(ctx, joined)
}

// openSecrets reads a stored secret column back into its secrets, newest
// first. A column that holds none is an error, not an endpoint that signs
// with nothing.
func (p *Plugin) openSecrets(ctx context.Context, stored string) ([]Secret, error) {
	plain := stored
	if p.sealer != nil {
		var err error
		if plain, err = p.sealer.Open(ctx, stored); err != nil {
			return nil, err
		}
	}
	var out []Secret
	for sec := range strings.FieldsSeq(plain) {
		out = append(out, Secret(sec))
	}
	if len(out) == 0 {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "webhooks: an endpoint holds no secret")
	}
	return out, nil
}

// errSecretsMoved rolls back a rotation another one overtook.
var errSecretsMoved = errors.New("webhooks: the endpoint's secrets changed meanwhile")

// RotateSecret makes next (a new random secret when "") the endpoint's
// newest secret and keeps the ones it had, so every delivery is signed
// with each of them (Standard Webhooks allows several signatures) and a
// receiver still set up with an older secret keeps verifying while it is
// switched over. Past MaxSecrets the oldest is dropped. Call
// RetireSecrets once every receiver has the new one. It returns the
// endpoint with its secrets; a next that is malformed, or already active,
// is CodeInvalidArgument; an unknown endpoint is CodeNotFound; a rotation
// racing another is CodeConflict, to retry.
func (p *Plugin) RotateSecret(ctx context.Context, s *records.Store, ref EndpointRef, next Secret) (Endpoint, error) {
	secret, err := p.secretOrNew(next)
	if err != nil {
		return Endpoint{}, err
	}
	return p.changeSecrets(ctx, s, ref, func(cur []Secret) ([]Secret, error) {
		for _, c := range cur {
			if c == secret {
				return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "webhooks: the secret is already active")
			}
		}
		out := append([]Secret{secret}, cur...)
		return out[:min(len(out), MaxSecrets)], nil
	})
}

// RetireSecrets drops every secret of the endpoint but its newest, ending
// a rotation, and returns the endpoint. An unknown endpoint is
// CodeNotFound.
func (p *Plugin) RetireSecrets(ctx context.Context, s *records.Store, ref EndpointRef) (Endpoint, error) {
	return p.changeSecrets(ctx, s, ref, func(cur []Secret) ([]Secret, error) { return cur[:1], nil })
}

// changeSecrets rewrites the endpoint's secrets with change, in one
// transaction. The write is conditional on the stored column being the
// one read, so two rotations racing never lose one's secret silently: the
// later is CodeConflict.
func (p *Plugin) changeSecrets(ctx context.Context, s *records.Store, ref EndpointRef, change func([]Secret) ([]Secret, error)) (Endpoint, error) {
	var ep Endpoint
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var stored string
		err := s.One(ctx, sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(EndpointSecret, &stored)).From(EndpointTable).Where(ref.where()...))
		if errors.Is(err, sqlb.ErrNoRows) {
			return filesystem.Errorf(filesystem.CodeNotFound, "webhooks: no endpoint %q", ref.ID)
		}
		if err != nil {
			return err
		}
		cur, err := p.openSecrets(ctx, stored)
		if err != nil {
			return err
		}
		next, err := change(cur)
		if err != nil {
			return err
		}
		if slices.Equal(next, cur) {
			// Nothing to write (retiring a single secret): MySQL would count
			// the unchanged row as 0 and the check below call it a race.
			eps, err := p.endpoints(ctx, s, ref.where()...)
			if err != nil || len(eps) != 1 {
				return errors.Join(err, errSecretsMoved)
			}
			ep = eps[0]
			return nil
		}
		sealed, err := p.sealSecrets(ctx, next)
		if err != nil {
			return err
		}
		n, err := s.Exec(ctx, sqlb.NewUpdate(s.Dialect()).Table(EndpointTable).Set(sqlb.Set(EndpointSecret, sealed)).
			Where(append(ref.where(), sqlb.Eq(EndpointSecret, stored))...))
		if err != nil {
			return err
		}
		if n != 1 {
			return filesystem.NewError(filesystem.CodeConflict, errSecretsMoved)
		}
		eps, err := p.endpoints(ctx, s, ref.where()...)
		if err != nil || len(eps) != 1 {
			return errors.Join(err, errSecretsMoved)
		}
		ep = eps[0]
		return nil
	})
	if err != nil {
		return Endpoint{}, err
	}
	return ep, nil
}
