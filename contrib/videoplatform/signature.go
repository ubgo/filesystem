package videoplatform

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"

	"github.com/ubgo/filesystem"
)

// VerifySignature checks a webhook signed the way Mux and Cloudflare
// Stream sign theirs: hex(HMAC-SHA256(secret, timestamp + "." + body)),
// with timestamp in Unix seconds within WebhookTolerance of now. It
// compares in constant time. A mismatch, a malformed value or a stale
// timestamp is CodePermission; an empty secret refuses everything, so a
// missing configuration never accepts unsigned requests.
func VerifySignature(secret []byte, timestamp string, body []byte, signature string, now time.Time) error {
	if len(secret) == 0 {
		return filesystem.Errorf(filesystem.CodePermission, "videoplatform: no webhook secret configured")
	}
	sec, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return filesystem.Errorf(filesystem.CodePermission, "videoplatform: malformed webhook timestamp")
	}
	if d := now.Sub(time.Unix(sec, 0)); d > WebhookTolerance || d < -WebhookTolerance {
		return filesystem.Errorf(filesystem.CodePermission, "videoplatform: webhook timestamp outside the tolerance")
	}
	got, err := hex.DecodeString(signature)
	if err != nil {
		return filesystem.Errorf(filesystem.CodePermission, "videoplatform: malformed webhook signature")
	}
	if !hmac.Equal(got, Sign(secret, timestamp, body)) {
		return filesystem.Errorf(filesystem.CodePermission, "videoplatform: webhook signature mismatch")
	}
	return nil
}

// Sign returns HMAC-SHA256(secret, timestamp + "." + body), the value
// VerifySignature expects hex encoded; fakes of a platform sign with it.
func Sign(secret []byte, timestamp string, body []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return mac.Sum(nil)
}
