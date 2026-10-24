package quota_test

import (
	"testing"
	"time"

	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/sqlb"
)

// TestWithClock pins quota.WithClock: usage rows are stamped with the
// plugin's own clock, in UTC at microsecond precision, not the Store's
// (which here is the wall clock, far from own).
func TestWithClock(t *testing.T) {
	own := time.Date(2041, 5, 6, 7, 8, 9, 123456789, time.FixedZone("x", 3600))
	e, _ := newEnv(t, fixed(quota.Limit{Bytes: 100, Files: 3}), quota.WithClock(func() time.Time { return own }))
	if _, err := e.add(t, "t1", "a", 10); err != nil {
		t.Fatal(err)
	}
	var stamped time.Time
	if err := e.store.One(t.Context(), sqlb.NewSelect(e.store.Dialect()).Columns(sqlb.Bind(quota.UpdatedAt, &stamped)).From(quota.Table).Where(sqlb.Eq(quota.Scope, "t1"))); err != nil {
		t.Fatal(err)
	}
	if want := own.UTC().Truncate(time.Microsecond); !stamped.Equal(want) {
		t.Fatalf("usage stamped %v, want the plugin's clock %v", stamped, want)
	}
}
