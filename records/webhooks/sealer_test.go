package webhooks_test

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/webhooks"
	"github.com/ubgo/sqlb"
)

// sealer is a stand-in for a KMS: it encodes the secret under a prefix.
type sealer struct{}

const sealedPrefix = "sealed:"

func (sealer) Seal(_ context.Context, secret string) (string, error) {
	return sealedPrefix + base64.RawURLEncoding.EncodeToString([]byte(secret)), nil
}

func (sealer) Open(_ context.Context, sealed string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, sealedPrefix))
	return string(b), err
}

// TestSealerKeepsSecretsOutOfTheTable: with WithSealer the table holds
// only the sealed text, and the plugin still reads the real secret back.
func TestSealerKeepsSecretsOutOfTheTable(t *testing.T) {
	ctx := t.Context()
	p, err := webhooks.New(webhooks.WithSealer(sealer{}), webhooks.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	s, err := records.New(testdb.Open(t), testdb.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ep, err := p.AddEndpoint(ctx, s, scope, "https://hooks.example.com/x")
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.One(ctx, sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(webhooks.EndpointSecret, &stored)).From(webhooks.EndpointTable)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, sealedPrefix) || strings.Contains(stored, ep.Secret.Reveal()) {
		t.Fatalf("the table holds %q", stored)
	}
	listed, err := p.Endpoints(ctx, s, scope)
	if err != nil || len(listed) != 1 || listed[0].Secret.Reveal() != ep.Secret.Reveal() {
		t.Fatalf("Endpoints = %v, %v; want the opened secret", listed, err)
	}
}

// flakySealer seals like sealer until down is set, then fails to seal.
type flakySealer struct {
	sealer
	down *bool
}

var errSealerDown = errors.New("kms down")

func (f flakySealer) Seal(ctx context.Context, secret string) (string, error) {
	if *f.down {
		return "", errSealerDown
	}
	return f.sealer.Seal(ctx, secret)
}

// TestRotateWithSealerDown: a rotation whose new secrets cannot be sealed
// fails with the sealer's error and leaves the endpoint's secrets as they
// were.
func TestRotateWithSealerDown(t *testing.T) {
	ctx := t.Context()
	down := false
	p, err := webhooks.New(webhooks.WithSealer(flakySealer{down: &down}), webhooks.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	s, err := records.New(testdb.Open(t), testdb.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ep, err := p.AddEndpoint(ctx, s, scope, "https://hooks.example.com/x")
	if err != nil {
		t.Fatal(err)
	}
	down = true
	if _, err := p.RotateSecret(ctx, s, webhooks.EndpointRef{Scope: scope, ID: ep.ID}, ""); !errors.Is(err, errSealerDown) {
		t.Fatalf("RotateSecret with the sealer down = %v", err)
	}
	listed, err := p.Endpoints(ctx, s, scope)
	if err != nil || len(listed) != 1 || len(listed[0].Secrets) != 1 || listed[0].Secrets[0].Reveal() != ep.Secret.Reveal() {
		t.Fatalf("after a failed rotation: %+v, %v", listed, err)
	}
}
