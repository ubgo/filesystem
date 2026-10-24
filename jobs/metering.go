package jobs

import (
	"context"
	"time"

	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/egress"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/filesystem/records/webhooks"
)

// Job names of metering and outbound events.
const (
	NameQuotaSweepReservations = "filesystem.quota_sweep_reservations"
	NameEgressReport           = "filesystem.egress_report"
	NameWebhookDeliver         = "filesystem.webhook_deliver"
	NameWebhookPrune           = "filesystem.webhook_prune"
)

// QuotaSweepReservations releases the quota reservations expired at the
// run's time (quota.Plugin.Sweep), so an upload that never finished gives
// its scope's space back. Each reservation carries its own expiry.
func QuotaSweepReservations(plugin *quota.Plugin, store *records.Store) Job {
	return Job{Name: NameQuotaSweepReservations, Run: func(ctx context.Context, now time.Time) (Result, error) {
		n, err := plugin.Sweep(ctx, store, now)
		return Result{Count: n}, err
	}}
}

// EgressReport emits the egress.daily_total events of every finished UTC
// day that grew since its last report (egress.Plugin.Report). Run it at
// least daily; more often costs one query when nothing grew.
func EgressReport(plugin *egress.Plugin, store *records.Store) Job {
	return Job{Name: NameEgressReport, Run: func(ctx context.Context, now time.Time) (Result, error) {
		n, err := plugin.Report(ctx, store, now)
		return Result{Count: int64(n)}, err
	}}
}

// WebhookDeliver attempts the webhook deliveries due at the run's time
// (webhooks.Plugin.Deliver); Count is how many were delivered. Its
// interval is the shortest wait between an event and its first attempt,
// and a retry waits for the first run after its backoff, so schedule it
// every few seconds to a minute.
func WebhookDeliver(plugin *webhooks.Plugin, store *records.Store) Job {
	return Job{Name: NameWebhookDeliver, Run: func(ctx context.Context, now time.Time) (Result, error) {
		n, err := plugin.Deliver(ctx, store, now)
		return Result{Count: int64(n)}, err
	}}
}

// WebhookPrune deletes delivered and failed webhook deliveries older than
// retention (webhooks.Plugin.Prune); pending ones are kept.
func WebhookPrune(plugin *webhooks.Plugin, store *records.Store, retention time.Duration) Job {
	return retentionJob(NameWebhookPrune, retention, func(ctx context.Context, cutoff time.Time) (int64, error) {
		return plugin.Prune(ctx, store, cutoff)
	})
}
