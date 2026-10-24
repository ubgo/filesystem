package rest

import (
	"encoding/json"
	"testing"

	"github.com/ubgo/filesystem"
)

// TestCheckExtra pins that an extra create field naming a reserved one,
// or holding invalid JSON, is refused, and that other fields pass.
func TestCheckExtra(t *testing.T) {
	ok := map[string]json.RawMessage{"video_quality": json.RawMessage(`"basic"`), "max": json.RawMessage(`3`)}
	if err := CheckExtra("mux", ok, "input", "passthrough"); err != nil {
		t.Fatalf("valid extras: %v", err)
	}
	for name, extra := range map[string]map[string]json.RawMessage{
		"reserved":     {"passthrough": json.RawMessage(`"x"`)},
		"invalid JSON": {"video_quality": json.RawMessage(`basic`)},
	} {
		if err := CheckExtra("mux", extra, "input", "passthrough"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestMerge pins that extra fields join base's, base's own fields win,
// and a base that is not a JSON object fails rather than being dropped.
func TestMerge(t *testing.T) {
	type base struct {
		URL  string `json:"url"`
		Mode string `json:"mode"`
	}
	out, err := Merge(base{URL: "https://x", Mode: "own"}, map[string]json.RawMessage{"mode": json.RawMessage(`"theirs"`), "extra": json.RawMessage(`1`)})
	if err != nil || string(out["mode"]) != `"own"` || string(out["extra"]) != `1` || string(out["url"]) != `"https://x"` {
		t.Fatalf("Merge = %v, %v", out, err)
	}
	if _, err := Merge(42, nil); err == nil {
		t.Error("a base that is not an object merged")
	}
	if _, err := Merge(make(chan int), nil); err == nil {
		t.Error("a base that does not marshal merged")
	}
}

// TestOverlay pins that a file's own create options win over the
// client's, that neither input map is changed, and that an empty per
// returns cfg itself while a nil cfg still takes per's fields.
func TestOverlay(t *testing.T) {
	cfg := map[string]json.RawMessage{"video_quality": json.RawMessage(`"basic"`), "max": json.RawMessage(`3`)}
	per := map[string]json.RawMessage{"video_quality": json.RawMessage(`"plus"`), "meta": json.RawMessage(`{}`)}
	got := Overlay(cfg, per)
	want := map[string]string{"video_quality": `"plus"`, "max": `3`, "meta": `{}`}
	if len(got) != len(want) {
		t.Fatalf("Overlay = %s", got)
	}
	for k, v := range want {
		if string(got[k]) != v {
			t.Errorf("Overlay[%s] = %s, want %s", k, got[k], v)
		}
	}
	if string(cfg["video_quality"]) != `"basic"` || len(cfg) != 2 || len(per) != 2 {
		t.Fatalf("Overlay changed its inputs: cfg %s, per %s", cfg, per)
	}
	if same := Overlay(cfg, nil); len(same) != 2 || string(same["video_quality"]) != `"basic"` {
		t.Fatalf("Overlay(cfg, nil) = %s", same)
	}
	if onlyPer := Overlay(nil, per); len(onlyPer) != 2 || string(onlyPer["video_quality"]) != `"plus"` {
		t.Fatalf("Overlay(nil, per) = %s", onlyPer)
	}
}
