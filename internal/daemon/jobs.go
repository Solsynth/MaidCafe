package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// jobRunner executes scheduled jobs: recurring invocations of configured
// actions or native operations. Schedules are standard five-field cron
// expressions or robfig descriptors ("@every 30s", "@hourly", "@daily").
// Runs go through the executor's concurrency slot, timeout and audit log
// with source "job", so scheduled executions are indistinguishable from
// manual ones in the audit trail; a failed run can publish a job.failure
// notification.
type jobRunner struct {
	executor  *WebhookExecutor
	ops       *nativeOpRunner
	publisher *atomic.Pointer[CloudPublisher]
	logger    *slog.Logger

	mu   sync.Mutex
	jobs map[string]*jobState
	wake chan struct{}
}

type jobState struct {
	cfg      config.JobConfig
	schedule cron.Schedule
	next     time.Time
	running  bool
}

func newJobRunner(executor *WebhookExecutor, ops *nativeOpRunner, publisher *atomic.Pointer[CloudPublisher], logger *slog.Logger) *jobRunner {
	if logger == nil {
		logger = slog.Default()
	}
	return &jobRunner{
		executor:  executor,
		ops:       ops,
		publisher: publisher,
		logger:    logger,
		jobs:      map[string]*jobState{},
		wake:      make(chan struct{}, 1),
	}
}

// SetJobs replaces the job table (hot reload). Runs already in flight finish;
// new schedules start from their next due time. Disabled jobs and jobs with
// invalid schedules are dropped with a warning.
func (r *jobRunner) SetJobs(jobs []config.JobConfig) {
	next := make(map[string]*jobState, len(jobs))
	for _, job := range jobs {
		if job.Enabled != nil && !*job.Enabled {
			continue
		}
		schedule, err := cron.ParseStandard(job.Schedule)
		if err != nil {
			r.logger.Warn("job schedule invalid; job skipped", "job", job.Name, "error", err)
			continue
		}
		next[job.Name] = &jobState{cfg: job, schedule: schedule, next: schedule.Next(time.Now())}
	}
	r.mu.Lock()
	r.jobs = next
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run ticks once a second and fires every due job until ctx is cancelled.
// A job whose previous run is still in flight is skipped for that tick (its
// next time advances), so overlapping runs never stack up.
func (r *jobRunner) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
			// Re-evaluate promptly after a SetJobs.
		case now := <-ticker.C:
			r.tick(ctx, now)
		}
	}
}

func (r *jobRunner) tick(ctx context.Context, now time.Time) {
	r.mu.Lock()
	var due []*jobState
	for _, state := range r.jobs {
		if !state.next.After(now) {
			due = append(due, state)
		}
	}
	r.mu.Unlock()
	for _, state := range due {
		r.runJob(ctx, state)
	}
}

func (r *jobRunner) runJob(ctx context.Context, state *jobState) {
	r.mu.Lock()
	if state.running {
		state.next = state.schedule.Next(time.Now())
		r.mu.Unlock()
		return
	}
	state.running = true
	state.next = state.schedule.Next(time.Now())
	r.mu.Unlock()

	body, _ := json.Marshal(state.cfg.Body)
	jobCtx, cancel := context.WithTimeout(ctx, r.timeoutFor(state.cfg))
	defer cancel()
	invokedBy := "job:" + state.cfg.Name

	var ok bool
	var stderr string
	if isNativeOpSlug(state.cfg.Action) {
		var params opParams
		if len(body) > 0 {
			var values map[string]any
			if json.Unmarshal(body, &values) == nil {
				params = nativeParamsFromValues(state.cfg.Action, values)
			}
		}
		response, _, requestErr := r.ops.dispatch(jobCtx, state.cfg.Action, params, "job", invokedBy)
		if requestErr != nil {
			stderr = requestErr.message
		} else {
			ok = response.OK
			stderr = response.Stderr
		}
	} else {
		response, requestErr := r.executor.RunAction(jobCtx, state.cfg.Action, body, "job", invokedBy)
		if requestErr != nil {
			stderr = requestErr.message
		} else {
			ok = response.OK
			stderr = response.Stderr
		}
	}

	r.mu.Lock()
	state.running = false
	r.mu.Unlock()

	if !ok && state.cfg.NotifyOnFailure {
		if p := r.publisher.Load(); p != nil {
			message := strings.TrimSpace(stderr)
			if len(message) > 4096 {
				message = message[:4096]
			}
			p.PublishNotification(context.Background(), notificationPayload{
				Kind:  "job.failure",
				Title: "Job " + state.cfg.Name + " failed",
				Body:  message,
				Metadata: map[string]any{
					"job":    state.cfg.Name,
					"action": state.cfg.Action,
				},
			})
		}
	}
}

// timeoutFor picks the job's own timeout, falling back to the current
// daemon-wide script timeout. The executor enforces its own bound as well;
// the tighter of the two wins.
func (r *jobRunner) timeoutFor(job config.JobConfig) time.Duration {
	if job.Timeout > 0 {
		return job.Timeout
	}
	return time.Duration(r.executor.scriptTimeout.Load())
}
