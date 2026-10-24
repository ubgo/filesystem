package pubsub

import (
	"testing"

	"github.com/ubgo/filesystem/contrib/notify"
)

// TestDecode_PassOther pins the event-kind seams: the name and the
// generation reach the event, another type is skipped by default and is
// KindOther with passOther, and the overwritten delete stays skipped.
func TestDecode_PassOther(t *testing.T) {
	attrs := func(typ string) map[string]string {
		return map[string]string{attrEventType: typ, attrBucket: "media", attrObject: "a.png", attrGeneration: "17"}
	}
	e, ok, err := decode("m1", attrs(eventFinalize), nil, false)
	if err != nil || !ok || e.Name != eventFinalize || e.VersionID != "17" {
		t.Fatalf("finalize %+v %v %v", e, ok, err)
	}
	if _, ok, _ := decode("m2", attrs("OBJECT_ARCHIVE"), nil, false); ok {
		t.Fatal("another type passed by default")
	}
	e, ok, err = decode("m3", attrs("OBJECT_ARCHIVE"), nil, true)
	if err != nil || !ok || e.Kind != notify.KindOther || e.Name != "OBJECT_ARCHIVE" {
		t.Fatalf("PassOther %+v %v %v", e, ok, err)
	}
	over := attrs(eventDelete)
	over[attrOverwritten] = "18"
	if _, ok, _ := decode("m4", over, nil, true); ok {
		t.Fatal("the overwritten delete passed")
	}
}
