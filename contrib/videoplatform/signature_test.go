package videoplatform_test

import (
	"encoding/hex"
	"strconv"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/videoplatform"
)

func TestVerifySignature(t *testing.T) {
	secret, body := []byte("whsec"), []byte(`{"type":"video.asset.ready"}`)
	now := time.Unix(1_800_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := hex.EncodeToString(videoplatform.Sign(secret, ts, body))
	if err := videoplatform.VerifySignature(secret, ts, body, sig, now.Add(time.Minute)); err != nil {
		t.Fatalf("a valid signature: %v", err)
	}
	stale := strconv.FormatInt(now.Add(-videoplatform.WebhookTolerance-time.Second).Unix(), 10)
	future := strconv.FormatInt(now.Add(videoplatform.WebhookTolerance+time.Second).Unix(), 10)
	for name, tc := range map[string]struct {
		secret  []byte
		ts, sig string
		body    []byte
	}{
		"no secret":       {nil, ts, hex.EncodeToString(videoplatform.Sign(nil, ts, body)), body},
		"tampered body":   {secret, ts, sig, []byte(`{"type":"video.asset.errored"}`)},
		"other secret":    {[]byte("other"), ts, sig, body},
		"replayed":        {secret, stale, hex.EncodeToString(videoplatform.Sign(secret, stale, body)), body},
		"from the future": {secret, future, hex.EncodeToString(videoplatform.Sign(secret, future, body)), body},
		"no timestamp":    {secret, "", sig, body},
		"not hex":         {secret, ts, "zz", body},
		"empty signature": {secret, ts, "", body},
	} {
		if err := videoplatform.VerifySignature(tc.secret, tc.ts, tc.body, tc.sig, now); filesystem.CodeOf(err) != filesystem.CodePermission {
			t.Errorf("%s: %v", name, err)
		}
	}
}
