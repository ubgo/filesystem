package remote

import "testing"

// TestMatchScheme pins the spec's wildcard: "*" matches any run,
// including none, anywhere in the scheme; the rest matches literally.
func TestMatchScheme(t *testing.T) {
	for _, tc := range []struct {
		scheme, url string
		want        bool
	}{
		{"https://www.youtube.com/watch*", "https://www.youtube.com/watch?v=1", true},
		{"https://www.youtube.com/watch*", "https://www.youtube.com/watch", true},
		{"https://www.youtube.com/watch*", "https://evil.example/?https://www.youtube.com/watch", false},
		{"https://*.vimeo.com/*", "https://player.vimeo.com/video/1", true},
		{"https://*.vimeo.com/*", "https://vimeo.com.evil.example/x", false},
		{"https://*.vimeo.com/*", "https://evil.example/.vimeo.com/x", false},
		{"https://*.vimeo.com/*", "https://evil.example?.vimeo.com/x", false},
		{"https://example.com", "https://example.com", true},
		{"https://example.com/a", "https://example.com/a", true},
		{"https://example.com/a", "https://example.com/ab", false},
		{"https://example.com/*/post/*", "https://example.com/u/post/9", true},
		{"https://example.com/*/post/*", "https://example.com/u/9", false},
	} {
		if got := matchScheme(tc.scheme, tc.url); got != tc.want {
			t.Errorf("matchScheme(%q, %q) = %v", tc.scheme, tc.url, got)
		}
	}
}

// TestEndpointBase pins what discovery compares: the canonical endpoint
// without its query, the "{format}" placeholder as json.
func TestEndpointBase(t *testing.T) {
	a, errA := endpointBase("https://Example.com:443/oembed.{format}?maxwidth=1")
	b, errB := endpointBase("https://example.com/oembed.json")
	if errA != nil || errB != nil || a != b {
		t.Errorf("%q (%v) != %q (%v)", a, errA, b, errB)
	}
}
