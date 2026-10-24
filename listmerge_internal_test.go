package filesystem

import (
	"context"
	"fmt"
	"testing"
)

// TestMergeListsDefaultsItsPageSize pins mergeLists' own default: a zero
// limit is a page of DefaultPageSize, like a Disk listing, rather than an
// empty page that would end a caller's paging loop at once. The Disk
// normalises the limit before any wrapper sees it, so this guards callers
// inside the package.
func TestMergeListsDefaultsItsPageSize(t *testing.T) {
	entries := make([]Entry, DefaultPageSize+1)
	for i := range entries {
		entries[i] = Entry{Key: fmt.Sprintf("%05d", i)}
	}
	src := listSource{page: func(context.Context, string) (Page, error) { return Page{Entries: entries}, nil }}
	page, err := mergeLists(t.Context(), []listSource{src}, ListOptions{}, firstHead)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != DefaultPageSize || page.NextCursor == "" {
		t.Fatalf("page of %d entries, next %q; want %d and a cursor", len(page.Entries), page.NextCursor, DefaultPageSize)
	}
}

// TestFirstHeadWithNoHead pins that a key no source holds is not shown.
func TestFirstHeadWithNoHead(t *testing.T) {
	if e, ok, err := firstHead(t.Context(), []*Entry{nil, nil}); ok || err != nil || e.Key != "" {
		t.Fatalf("firstHead(nil, nil) = %+v, %t, %v; want nothing shown", e, ok, err)
	}
}

// TestDataURITypeWithoutAnyType pins the fallback when neither the stored
// nor the detected type parses: the URI is application/octet-stream, never
// an empty or malformed media type.
func TestDataURITypeWithoutAnyType(t *testing.T) {
	if got := dataURIType("", ""); got != "application/octet-stream" {
		t.Fatalf("dataURIType = %q", got)
	}
}
