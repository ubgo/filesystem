# jobs/asynq

`github.com/ubgo/filesystem/jobs/asynq` runs the maintenance jobs of [`jobs`](../README.md) on [Asynq](https://github.com/hibiken/asynq), the Redis task queue, for applications that already run Asynq. One task type (`filesystem:maintenance`, the payload is the job's name) executes every job; each `jobs.Schedule` becomes an Asynq scheduler entry, and every run is unique while it is queued, so a fleet of schedulers and servers never runs one job twice at once.

```go
handler, err := fsasynq.NewHandler(fsasynq.HandlerOptions{OnRun: logReport}, jobList...)
mux := asynq.NewServeMux()
mux.Handle(fsasynq.TaskType, handler)
scheduler := asynq.NewScheduler(redisOpt, nil)
err = fsasynq.Register(ctx, scheduler, client, schedules...)
```

| Name | What it is |
|---|---|
| `NewHandler(HandlerOptions{OnRun, Now}, jobs...)` | the Asynq handler for these jobs; an unnamed job, one without `Run`, or a repeated name is `CodeInvalidArgument` |
| `Handler.ProcessTask` | runs the job named in the payload; an unknown name is not retried (`asynq.SkipRetry`, `CodeNotFound`); a job's error is returned so Asynq retries |
| `Register(ctx, scheduler, client, schedules...)` | an `@every <Every>` entry per schedule, unique for one interval; `RunAtStart` ones are enqueued at once through `client`; `Jitter` is not used; a non-positive interval is `CodeInvalidArgument` |
| `RegisterWith(ctx, scheduler, client, RegisterOptions{Queue, MaxRetry, Timeout, Spec, Unique}, schedules...)`, `EnqueueWith(ctx, client, name, RegisterOptions)` | a dedicated queue, a retry budget (0 Asynq's default, negative none) and a timeout; `Spec` returns a schedule's cronspec (`"0 3 * * *"`), which a `Schedule.Next` schedule needs (`unsupported` without it, since Asynq schedules by cronspec); `Unique(job)` returns how long a queued run blocks a second one (nil or 0 keeps the interval, else one hour; under one second, negative included, is `CodeInvalidArgument`, since uniqueness is what stops two servers running one job) |
| `Enqueue(ctx, client, name)` | queues one run now; a no-op while a run of that job queued in the last hour exists (`EnqueueWith` with `Unique` changes the window) |
| `NewTask(name)`, `TaskType` | the task of one job, and its type |

## Tests

The unit tests need nothing. `TestOnRedis` runs a real Asynq server when `FILESYSTEM_ASYNQ_REDIS_ADDR` names a throwaway Redis (the repository's Asynq task starts one); it enqueues to Asynq's default queue there, so never point it at a shared server.
