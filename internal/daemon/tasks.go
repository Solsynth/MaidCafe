package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Long native operations — image pulls, and the compose pulls and recreates
// that need them — run as tasks instead of inside the request that asked for
// them. A task outlives its caller: a browser tab that navigates away, a
// phone that locks, an HTTP client whose own read timeout fires long before a
// pull finishes (MaidKit's gives up after ten seconds of silence). The client
// gets the task's id back immediately and watches it, which is also where
// progress comes from: a task keeps the output its stages produce, and a
// client asks for the bytes after the last one it read.
//
// Tasks are in memory. They are a view of work in flight, not a durable job
// queue: a daemon restart loses them along with the runs themselves, which is
// why they are not what a scheduler should build on.
const (
	taskStatusRunning   = "running"
	taskStatusSucceeded = "succeeded"
	taskStatusFailed    = "failed"
	taskStatusCanceled  = "canceled"

	// stageStatusPending is a stage the plan has not reached yet: the whole
	// plan is known when the task starts, so a client sees what is still to
	// come, not only what has happened.
	stageStatusPending = "pending"
)

// taskOutputLimit is how much output a task retains. Compose prints a line per
// layer and per container, so a tail answers "where is this run" — and a
// client that falls further behind than this resynchronizes with the tail
// rather than the bytes it missed.
const taskOutputLimit = 32 * 1024

// taskHistoryLimit is how many finished tasks stay queryable. Running tasks are
// never dropped: a task is the only handle to a run in flight.
const taskHistoryLimit = 32

// opTaskStage is one step of a task's plan. A stage is the unit an operator
// reasons about ("pull", "recreate"), so it is also the unit progress is
// reported in.
type opTaskStage struct {
	// Label names the stage in the terms its operation uses. Clients
	// translate the labels they know and show the rest as they are.
	Label      string     `json:"label"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// opTaskView is a task as the API serves it: the outcome in the same shape a
// synchronous native operation returns ([executionResponse]), plus what a
// watcher needs to follow it — the plan, where it is in the plan, and a byte
// count to ask for output from.
type opTaskView struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	DisplayName string        `json:"display_name"`
	Target      string        `json:"target"`
	Source      string        `json:"source"`
	InvokedBy   string        `json:"invoked_by,omitempty"`
	Status      string        `json:"status"`
	StartedAt   time.Time     `json:"started_at"`
	FinishedAt  *time.Time    `json:"finished_at,omitempty"`
	Stages      []opTaskStage `json:"stages"`
	OK          bool          `json:"ok"`
	ExitCode    int           `json:"exit_code"`
	Stdout      string        `json:"stdout"`
	Stderr      string        `json:"stderr"`
	Error       string        `json:"error,omitempty"`
	// OutputBytes counts everything the run has written, so a client can ask
	// for the bytes after the last one it read instead of the whole tail.
	OutputBytes int64 `json:"output_bytes"`
}

// opTask is one running or finished native operation.
type opTask struct {
	mu          sync.Mutex
	id          string
	name        string
	displayName string
	target      string
	source      string
	invokedBy   string
	status      string
	startedAt   time.Time
	finishedAt  *time.Time
	stages      []opTaskStage
	ok          bool
	exitCode    int
	stdout      string
	stderr      string
	failure     string
	// output is the retained tail of everything the run's commands wrote;
	// written is how much was written in total, which is the offset a client
	// tracks.
	output  []byte
	written int64
	// cancel stops the run, and done closes when it has stopped.
	cancel context.CancelFunc
	done   chan struct{}
}

func newOpTask(id, name, displayName, target, source, invokedBy string, stages []opStage, cancel context.CancelFunc) *opTask {
	planned := make([]opTaskStage, 0, len(stages))
	for _, stage := range stages {
		planned = append(planned, opTaskStage{Label: stage.label, Status: stageStatusPending})
	}
	return &opTask{
		id:          id,
		name:        name,
		displayName: displayName,
		target:      target,
		source:      source,
		invokedBy:   invokedBy,
		status:      taskStatusRunning,
		startedAt:   time.Now().UTC(),
		stages:      planned,
		cancel:      cancel,
		done:        make(chan struct{}),
	}
}

// newTaskID returns a task's opaque handle. It is random rather than
// sequential so it cannot be walked, and hex so it survives every transport
// this daemon speaks.
func newTaskID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail on the platforms this daemon builds for;
		// a timestamp still leaves the task addressable if it somehow does.
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// stageStarted marks [label] as the stage now running.
func (t *opTask) stageStarted(label string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now().UTC()
	for i := range t.stages {
		if t.stages[i].Label == label && t.stages[i].Status == stageStatusPending {
			t.stages[i].Status = taskStatusRunning
			t.stages[i].StartedAt = &now
			return
		}
	}
}

// stageFinished closes out [label]. A stage that failed stops the plan, so the
// stages after it stay pending.
func (t *opTask) stageFinished(label string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now().UTC()
	for i := range t.stages {
		if t.stages[i].Label != label || t.stages[i].Status != taskStatusRunning {
			continue
		}
		if ok {
			t.stages[i].Status = taskStatusSucceeded
		} else {
			t.stages[i].Status = taskStatusFailed
		}
		t.stages[i].FinishedAt = &now
		return
	}
}

// Write appends command output to the task's tail. It is the sink the
// executor streams each stage's stdout and stderr into, so it must tolerate
// both streams writing at once.
func (t *opTask) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.output = append(t.output, p...)
	if len(t.output) > taskOutputLimit {
		t.output = append(t.output[:0], t.output[len(t.output)-taskOutputLimit:]...)
	}
	t.written += int64(len(p))
	return len(p), nil
}

// finish closes the task out with its result. [timeout] is the bound the run
// was given, so a run that hit it says so rather than reporting an exit code
// the command never returned.
func (t *opTask) finish(response executionResponse, ctxErr error, timeout time.Duration) {
	t.mu.Lock()
	now := time.Now().UTC()
	t.finishedAt = &now
	t.ok = response.OK
	t.exitCode = response.ExitCode
	t.stdout = response.Stdout
	t.stderr = response.Stderr
	switch {
	case ctxErr == nil && response.OK:
		t.status = taskStatusSucceeded
		t.failure = ""
	case errors.Is(ctxErr, context.Canceled):
		t.status = taskStatusCanceled
		t.failure = "the run was cancelled"
	case errors.Is(ctxErr, context.DeadlineExceeded):
		t.status = taskStatusFailed
		t.failure = fmt.Sprintf("the run timed out after %s", timeout)
	default:
		t.status = taskStatusFailed
		t.failure = auditErrorTail(response)
	}
	t.mu.Unlock()
	close(t.done)
}

// cancelRun stops a running task, reporting whether it was running to stop.
func (t *opTask) cancelRun() bool {
	t.mu.Lock()
	running := t.status == taskStatusRunning
	cancel := t.cancel
	t.mu.Unlock()
	if !running || cancel == nil {
		return false
	}
	cancel()
	return true
}

// wait blocks until the run has stopped.
func (t *opTask) wait() { <-t.done }

func (t *opTask) finished() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status != taskStatusRunning
}

// snapshot returns the task's state, safe to hand to a caller that is not
// holding its lock.
func (t *opTask) snapshot() opTaskView {
	t.mu.Lock()
	defer t.mu.Unlock()
	stages := make([]opTaskStage, len(t.stages))
	copy(stages, t.stages)
	return opTaskView{
		ID:          t.id,
		Name:        t.name,
		DisplayName: t.displayName,
		Target:      t.target,
		Source:      t.source,
		InvokedBy:   t.invokedBy,
		Status:      t.status,
		StartedAt:   t.startedAt,
		FinishedAt:  t.finishedAt,
		Stages:      stages,
		OK:          t.ok,
		ExitCode:    t.exitCode,
		Stdout:      t.stdout,
		Stderr:      t.stderr,
		Error:       t.failure,
		OutputBytes: t.written,
	}
}

// outputSince returns the retained output a client should append to what it
// already has, given the total byte count it last read. A client that has
// fallen behind the retained tail gets the tail instead, and is told so: what
// it holds must be replaced rather than extended.
func (t *opTask) outputSince(since int64) (chunk []byte, from int64, truncated bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	from = t.written - int64(len(t.output))
	if since < 0 {
		return append([]byte(nil), t.output...), from, false
	}
	if since < from {
		return append([]byte(nil), t.output...), from, true
	}
	if since > t.written {
		since = t.written
	}
	offset := since - from
	if offset > int64(len(t.output)) {
		offset = int64(len(t.output))
	}
	return append([]byte(nil), t.output[offset:]...), since, false
}

// taskStore holds the daemon's recent tasks. It is the only handle a client
// has to a run in flight and to the output the run produced.
type taskStore struct {
	mu    sync.Mutex
	byID  map[string]*opTask
	order []string // newest first
}

func newTaskStore() *taskStore {
	return &taskStore{byID: make(map[string]*opTask)}
}

func (s *taskStore) add(task *opTask) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[task.id] = task
	s.order = append([]string{task.id}, s.order...)
	finished := 0
	for _, id := range s.order {
		if existing, ok := s.byID[id]; ok && existing.finished() {
			finished++
		}
	}
	for i := len(s.order) - 1; i >= 0 && finished > taskHistoryLimit; i-- {
		id := s.order[i]
		existing, ok := s.byID[id]
		if !ok || !existing.finished() {
			continue
		}
		delete(s.byID, id)
		s.order = append(s.order[:i], s.order[i+1:]...)
		finished--
	}
}

// cancelAll stops every running task. It is what the daemon's shutdown calls:
// a task outlives the client that started it, not the daemon that runs it.
func (s *taskStore) cancelAll() {
	s.mu.Lock()
	running := make([]*opTask, 0, len(s.byID))
	for _, task := range s.byID {
		running = append(running, task)
	}
	s.mu.Unlock()
	for _, task := range running {
		task.cancelRun()
	}
}

func (s *taskStore) get(id string) (*opTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.byID[id]
	return task, ok
}

// list returns the most recent tasks, newest first, at most [limit] of them.
func (s *taskStore) list(limit int) []opTaskView {
	s.mu.Lock()
	ordered := make([]*opTask, 0, len(s.order))
	for _, id := range s.order {
		if task, ok := s.byID[id]; ok {
			ordered = append(ordered, task)
		}
	}
	s.mu.Unlock()
	if limit <= 0 || limit > len(ordered) {
		limit = len(ordered)
	}
	views := make([]opTaskView, 0, limit)
	for _, task := range ordered[:limit] {
		views = append(views, task.snapshot())
	}
	return views
}
