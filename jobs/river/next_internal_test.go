package river

import (
	"testing"
	"time"

	"github.com/ubgo/filesystem/jobs/diskjobs"
)

// TestPeriodicSchedule pins that a Next schedule reaches River as its
// own calendar, and an Every schedule as River's interval.
func TestPeriodicSchedule(t *testing.T) {
	start := time.Date(2026, 10, 9, 1, 0, 0, 0, time.UTC)
	nightly := periodicSchedule(diskjobs.Schedule{Next: diskjobs.Daily(3, 0, nil)})
	if got := nightly.Next(start); !got.Equal(time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("Next schedule: %s", got)
	}
	hourly := periodicSchedule(diskjobs.Schedule{Every: time.Hour})
	if got := hourly.Next(start); !got.Equal(start.Add(time.Hour)) {
		t.Fatalf("Every schedule: %s", got)
	}
}
