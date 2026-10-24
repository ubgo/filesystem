package jobs

import (
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/jobs/diskjobs"
)

// The scheduler and Promote live in diskjobs, which needs no database;
// these aliases keep their jobs.* names, so code written against either
// package compiles against both.
type (
	// Schedule runs a Job on an interval or a calendar; see
	// diskjobs.Schedule.
	Schedule = diskjobs.Schedule
	// Report is one finished (or skipped) run; see diskjobs.Report.
	Report = diskjobs.Report
	// SchedulerOptions configure a Scheduler; see
	// diskjobs.SchedulerOptions.
	SchedulerOptions = diskjobs.SchedulerOptions
	// Scheduler runs Schedules in-process; see diskjobs.Scheduler.
	Scheduler = diskjobs.Scheduler
)

// ErrRunning reports a run skipped because the same job was still
// running; see diskjobs.ErrRunning.
var ErrRunning = diskjobs.ErrRunning

// NamePromote is the default name of Promote's job.
const NamePromote = diskjobs.NamePromote

// NewScheduler returns a Scheduler; see diskjobs.NewScheduler.
func NewScheduler(opts SchedulerOptions) *Scheduler { return diskjobs.NewScheduler(opts) }

// Promote moves uploads from a fast staging disk to the durable store;
// see diskjobs.Promote.
func Promote(staging *filesystem.Disk, dir string, store *filesystem.Disk, minAge time.Duration) Job {
	return diskjobs.Promote(staging, dir, store, minAge)
}
