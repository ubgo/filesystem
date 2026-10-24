package upload_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// legacyStore drops Recorded.ContentType, as a store written before the
// field existed would.
type legacyStore struct{ upload.IdempotencyStore }

func (l legacyStore) Complete(ctx context.Context, key string, r upload.Recorded) error {
	r.ContentType = ""
	return l.IdempotencyStore.Complete(ctx, key, r)
}

// TestIdempotentReplayKeepsContentType pins R-C6: a replay answers in the
// media type the first response's SuccessWriter chose, the default stays
// application/json, and a record without a type replays as JSON.
func TestIdempotentReplayKeepsContentType(t *testing.T) {
	const apiType = "application/vnd.api+json"
	envelope := func(w http.ResponseWriter, _ *http.Request, status int, body upload.SuccessBody) {
		w.Header().Set("Content-Type", apiType)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": body})
	}
	body := func() (io.Reader, string) {
		return strings.NewReader(`{"filename":"a.png","content_type":"image/png","size":10}`), "application/json"
	}
	newStore := func() upload.IdempotencyStore {
		s, err := upload.NewMemoryIdempotency(upload.MemoryIdempotencyOptions{TTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	cases := []struct {
		name    string
		store   upload.IdempotencyStore
		respond upload.SuccessWriter
		want    string
	}{
		{"custom envelope", newStore(), envelope, apiType},
		{"default", newStore(), nil, "application/json"},
		{"record without a type", legacyStore{newStore()}, envelope, "application/json"},
	}
	for _, c := range cases {
		h := upload.PresignHandlerWith(newDisk(t, presigning{memory.New()}), images, time.Minute, upload.PresignHandlerOptions{
			Idempotency: &upload.Idempotency{Store: c.store, Owner: byUser},
			Respond:     c.respond,
		})
		first := postWithKey(t, h, body, "k1", "u1")
		again := postWithKey(t, h, body, "k1", "u1")
		if again.Code != http.StatusOK || again.Header().Get(upload.HeaderIdempotentReplayed) != "true" || again.Body.String() != first.Body.String() {
			t.Errorf("%s: replay %d %v %q, first %q", c.name, again.Code, again.Header(), again.Body, first.Body)
		}
		if got := again.Header().Get("Content-Type"); got != c.want {
			t.Errorf("%s: replay Content-Type %q, want %q", c.name, got, c.want)
		}
	}
}
