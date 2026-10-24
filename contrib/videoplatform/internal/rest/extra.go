package rest

import (
	"encoding/json"
	"maps"
	"slices"

	"github.com/ubgo/filesystem"
)

// CheckExtra refuses extra create-request fields that name one of the
// adapter's own (reserved), since the adapter sets those and an override
// would change what it relies on (the source URL, the playback policy,
// the passthrough that maps a webhook back to its file), or that are not
// valid JSON. Any failure is CodeInvalidArgument naming the field.
func CheckExtra(platform string, extra map[string]json.RawMessage, reserved ...string) error {
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		if slices.Contains(reserved, k) {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "%s: create option %q is set by the adapter", platform, k)
		}
		if !json.Valid(extra[k]) {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "%s: create option %q is not valid JSON", platform, k)
		}
	}
	return nil
}

// Merge returns base's JSON fields plus extra's, for a create request
// with the platform's own options. CheckExtra has refused an extra field
// that names one of base's; base's fields win regardless.
func Merge[T any](base T, extra map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	b, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	for k, v := range extra {
		if _, own := out[k]; !own {
			out[k] = v
		}
	}
	return out, nil
}

// Overlay returns cfg's extra fields with per's on top, per winning a
// field both set: a file's own create options over the client's. Neither
// map is changed; nil per returns cfg as is.
func Overlay(cfg, per map[string]json.RawMessage) map[string]json.RawMessage {
	if len(per) == 0 {
		return cfg
	}
	out := maps.Clone(cfg)
	if out == nil {
		out = make(map[string]json.RawMessage, len(per))
	}
	maps.Copy(out, per)
	return out
}
