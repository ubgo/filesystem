package gcs_test

import (
	"reflect"
	"testing"
	"time"

	"cloud.google.com/go/storage"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gcs"
)

// TestOnlyAgeAndPrefixCoversEveryField sets each field of
// storage.LifecycleCondition in turn, found by reflection so a field a
// later SDK adds is covered too, and proves only the age and the prefixes
// are accepted.
func TestOnlyAgeAndPrefixCoversEveryField(t *testing.T) {
	typ := reflect.TypeFor[storage.LifecycleCondition]()
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		var c storage.LifecycleCondition
		v := reflect.ValueOf(&c).Elem().Field(i)
		switch {
		case f.Type == reflect.TypeFor[time.Time]():
			v.Set(reflect.ValueOf(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
		case v.Kind() == reflect.Bool:
			v.SetBool(true)
		case v.CanInt():
			v.SetInt(1)
		case v.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.String:
			v.Set(reflect.ValueOf([]string{"x"}).Convert(f.Type))
		default:
			t.Fatalf("field %s of kind %s: teach this test to set it", f.Name, v.Kind())
		}
		modelled := f.Name == "AgeInDays" || f.Name == "MatchesPrefix"
		if got := gcs.OnlyAgeAndPrefix(c); got != modelled {
			t.Errorf("%s set: onlyAgeAndPrefix = %v, want %v", f.Name, got, modelled)
		}
	}
}

// TestModeOfUnknownRPO proves an RPO value that is neither mode is an
// error naming it, never "no mode".
func TestModeOfUnknownRPO(t *testing.T) {
	if m, err := gcs.ModeOf(storage.RPOUnknown); err != nil || m != "" {
		t.Errorf("no RPO = %q, %v", m, err)
	}
	if m, err := gcs.ModeOf(storage.RPOAsyncTurbo); err != nil || m != gcs.ReplicationTurbo {
		t.Errorf("turbo = %q, %v", m, err)
	}
	if m, err := gcs.ModeOf(storage.RPOAsyncTurbo + 1); filesystem.CodeOf(err) != filesystem.CodeUnsupported || m != "" {
		t.Errorf("unknown RPO = %q, %v", m, err)
	}
}
