# Bucket notifications: confirm direct uploads from the bucket's events

A browser that uploads straight to the bucket is supposed to tell your API when it has finished, so the server can check the file with `upload.Confirm`. A browser that closes the tab never does. This program has the bucket tell you instead: S3 publishes an event to an SQS queue for every object written under `incoming/`, and a consumer built with `contrib/notify/sqs` confirms each new object against the upload policy the moment it lands, deleting what the policy refuses. The bucket is a memory disk and the queue is an in-memory implementation of the consumer's `sqs.API` interface, so it needs no AWS account. It uses the core `filesystem` package, `memory`, `upload`, `contrib/notify` and `contrib/notify/sqs`. The guide is [Maintenance, bucket notifications](../../docs/maintenance/README.md#bucket-notifications).

## Run it

```sh
cd examples
go run ./maintenance-notify
go test ./maintenance-notify
```

It needs no environment variables, no network and no files.

## What it does

```text
left for redelivery: sqs: message m1: database unavailable
refused incoming/avatar-2.png: rejected, object deleted
rejected an event from bucket "other-bucket"
dropped a message that is not an S3 event
poll 1: 3 acknowledged, 1 left in the queue
confirmed incoming/avatar 1.png: image/png, 72 bytes
poll 2: 1 acknowledged, 0 left in the queue
duplicate incoming/avatar 1.png: skipped
poll 3: 1 acknowledged, 0 left in the queue
stored: [incoming/avatar 1.png]
```

Two clients upload straight to the bucket: a real PNG, and an HTML page named `.png`. The queue then holds four messages: the two events, an event from a bucket this consumer does not serve, and a message that is not an S3 event at all.

- The first poll fails the PNG's event on purpose, as a database outage would: the handler returns an error, so the message stays in the queue. The HTML file is refused by `upload.Confirm`, which reads the stored bytes, and deleted. The foreign bucket's event is acknowledged without being handled, and the stray message is dropped.
- The second poll receives the PNG's event again and confirms it. Its key has a space, which S3 sends URL-encoded as `+`; the consumer decodes it.
- The queue then delivers the same event a third time, as SQS may (it delivers at least once). The handler has seen its `Event.ID` and skips it.

## Key code

The handler confirms objects created under `incoming/` and remembers the events it handled:

<!-- ds:block id=confirmer-handle-9z5tacrg -->
**Confirmer.Handle** · [`examples/maintenance-notify/main.go:70-95`](main.go#L70-L95)

```go
func (c *Confirmer) Handle(ctx context.Context, e notify.Event) error {
	if e.Kind != notify.KindCreated || !filesystem.KeyInDir(e.Key, incomingDir) {
		return nil
	}
	if c.seen[e.ID] {
		fmt.Fprintf(c.Out, "duplicate %s: skipped\n", e.Key)
		return nil
	}
	if err := c.fail; err != nil {
		c.fail = nil
		return err
	}
	res, err := upload.Confirm(ctx, c.Disk, e.Key, path.Base(e.Key), avatars)
	switch {
	case filesystem.CodeOf(err) == filesystem.CodeRejected || filesystem.CodeOf(err) == filesystem.CodeTooLarge:
		fmt.Fprintf(c.Out, "refused %s: %s, object deleted\n", e.Key, filesystem.CodeOf(err))
	case filesystem.CodeOf(err) == filesystem.CodeNotFound:
		fmt.Fprintf(c.Out, "gone %s: deleted before the event arrived\n", e.Key)
	case err != nil:
		return err // anything else is worth another delivery
	default:
		fmt.Fprintf(c.Out, "confirmed %s: %s, %d bytes\n", res.Key, res.ContentType, res.Size)
	}
	c.seen[e.ID] = true
	return nil
}
```
<!-- /ds:block hash=95ae1a -->

The consumer names the one bucket whose events it handles:

<!-- ds:block id=newconsumer-9gz6djdp -->
**newConsumer** · [`examples/maintenance-notify/main.go:101-117`](main.go#L101-L117)

```go
func newConsumer(q sqs.API, c *Confirmer, out io.Writer) (*sqs.Consumer, error) {
	return sqs.New(q, sqs.Options{
		QueueURL: queueURL,
		Buckets:  notify.Buckets{bucketName},
		Handler:  c.Handle,
		OnRejected: func(e notify.Event) {
			fmt.Fprintf(out, "rejected an event from bucket %q\n", e.Bucket)
		},
		OnError: func(err error) {
			if errors.Is(err, notify.ErrMalformed) {
				fmt.Fprintln(out, "dropped a message that is not an S3 event")
				return
			}
			fmt.Fprintf(out, "left for redelivery: %v\n", err)
		},
	})
}
```
<!-- /ds:block hash=1ccd3a -->

## Going to production

- Replace the in-memory queue with `*sqs.Client` from `aws-sdk-go-v2`, and call `consumer.Run(ctx)` in a goroutine instead of `Poll`.
- Keep the set of handled events in your database (a unique `Event.ID` column), not in memory, so a restart or a second instance does not confirm twice.
- Configure the bucket's event notification for `s3:ObjectCreated:*` on the `incoming/` prefix, and give the queue a dead-letter queue.
- GCS (Pub/Sub) and Azure (Event Grid) have consumers of their own in `contrib/notify/pubsub` and `contrib/notify/eventgrid`, with the same `notify.Handler`.
