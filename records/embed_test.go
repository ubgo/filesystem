package records_test

import (
	"encoding/json"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// TestEmbedRoundTrip: a file embedded on an owner row reads back with
// what a page needs, and the JSON carries its version.
func TestEmbedRoundTrip(t *testing.T) {
	e := newEnv(t, nil)
	f, err := e.store.Upload(t.Context(), "t", "media", reader(pngData), "cover.png", images)
	if err != nil {
		t.Fatal(err)
	}
	f.Metadata = map[string]string{"alt": "a cat"}
	raw, err := records.Embed(f)
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.V != records.EmbedVersion {
		t.Fatalf("version in %s = %d, %v", raw, probe.V, err)
	}
	got, err := records.Unembed(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != f.ID || got.Key != f.Key || got.Size != f.Size || got.SHA256 != f.SHA256 || got.Visibility != f.Visibility ||
		got.ContentType != f.ContentType || !got.CreatedAt.Equal(f.CreatedAt) || got.Metadata["alt"] != "a cat" {
		t.Fatalf("round trip: %+v, want %+v", got, f)
	}
	if _, err := e.store.URL(t.Context(), got, 60); err != nil {
		t.Fatalf("an unembedded file cannot be linked: %v", err)
	}
}

// TestUnembedRefuses: malformed, unversioned and newer JSON is refused.
func TestUnembedRefuses(t *testing.T) {
	for raw, code := range map[string]filesystem.Code{
		`not json`:              filesystem.CodeInvalidArgument,
		`{"id":"fil_x"}`:        filesystem.CodeInvalidArgument,
		`{"v":1}`:               filesystem.CodeInvalidArgument,
		`{"v":99,"id":"fil_x"}`: filesystem.CodeUnsupported,
	} {
		if _, err := records.Unembed(json.RawMessage(raw)); filesystem.CodeOf(err) != code {
			t.Errorf("%s: %v, want %s", raw, err, code)
		}
	}
}
