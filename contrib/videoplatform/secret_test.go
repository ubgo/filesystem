package videoplatform_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem/contrib/videoplatform"
	"github.com/ubgo/filesystem/contrib/videoplatform/cfstream"
	"github.com/ubgo/filesystem/contrib/videoplatform/mux"
)

// TestSecret_NeverPrints pins the redaction: the adapters' clients and
// configs, printed with every verb fmt has and logged through slog's text
// and JSON handlers, never show a token or webhook secret, while the
// adapters still read the value.
func TestSecret_NeverPrints(t *testing.T) {
	const token, hook = "tok-SECRET-1", "whsec-SECRET-2"
	muxCfg := mux.Config{TokenID: "id", TokenSecret: videoplatform.NewSecret(token), WebhookSecret: videoplatform.NewSecret(hook)}
	cfCfg := cfstream.Config{AccountID: "acct", APIToken: videoplatform.NewSecret(token), WebhookSecret: videoplatform.NewSecret(hook)}
	muxClient, err := mux.New(muxCfg)
	if err != nil {
		t.Fatal(err)
	}
	cfClient, err := cfstream.New(cfCfg)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"secret": muxCfg.TokenSecret, "mux.Config": muxCfg, "mux.Client": muxClient, "cfstream.Config": cfCfg, "cfstream.Client": cfClient}
	for name, v := range values {
		var logs bytes.Buffer
		slog.New(slog.NewTextHandler(&logs, nil)).Info("m", "v", v)
		slog.New(slog.NewJSONHandler(&logs, nil)).Info("m", "v", v)
		for _, out := range []string{fmt.Sprintf("%v", v), fmt.Sprintf("%+v", v), fmt.Sprintf("%#v", v), fmt.Sprintf("%s", v), fmt.Sprint(v), logs.String()} {
			if strings.Contains(out, "SECRET") {
				t.Errorf("%s leaks a secret: %s", name, out)
			}
		}
	}
	if string(muxCfg.TokenSecret.Reveal()) != token || !videoplatform.NewSecret("").IsZero() || (videoplatform.Secret{}).Reveal() != nil {
		t.Fatal("Reveal or IsZero is wrong")
	}
}

// TestDurationOf pins the clamp of a platform's seconds: a figure past
// the longest Duration must not wrap negative (it does on amd64 without
// it); run with GOARCH=amd64 too.
func TestDurationOf(t *testing.T) {
	for in, want := range map[float64]time.Duration{2.5: 2500 * time.Millisecond, -1: 0, 0: 0, math.NaN(): 0, 1e22: math.MaxInt64, math.Inf(1): math.MaxInt64} {
		if got := videoplatform.DurationOf(in); got != want {
			t.Errorf("DurationOf(%g) = %v, want %v", in, got, want)
		}
	}
}
