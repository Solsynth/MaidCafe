package daemon

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// The task API: what a client needs to follow a long native operation. The
// operations are started through the routes that already existed — the ones
// that pull an image answer 202 with a task instead of holding the request —
// and from there a client polls one task for its stages and for the output it
// has not read yet.

// taskListLimit is the largest page of tasks a client may ask for.
const taskListLimit = 64

// taskResponse is one task plus the part of its output the caller has not read.
type taskResponse struct {
	OK   bool       `json:"ok"`
	Task opTaskView `json:"task"`
	// Output is what the run wrote from [OutputFrom] on; a client appends it to
	// what it already has. [OutputTruncated] says the bytes before it are gone
	// — a task retains a tail — so the buffer must be replaced rather than
	// extended.
	Output          string `json:"output"`
	OutputFrom      int64  `json:"output_from"`
	OutputTruncated bool   `json:"output_truncated,omitempty"`
}

// handleTaskGet serves one task, with the output written since ?since=<bytes>.
// Omitting `since` returns the retained tail, which is what a client that just
// opened a view on a task wants.
func (a *App) handleTaskGet(c *gin.Context) {
	task, ok := a.tasks.get(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"ok": false, "error": "no such task"})
		return
	}
	since := int64(-1)
	if raw := c.Query("since"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "since must be a byte count within the task"})
			return
		}
		since = value
	}
	chunk, from, truncated := task.outputSince(since)
	c.JSON(http.StatusOK, taskResponse{
		OK:              true,
		Task:            task.snapshot(),
		Output:          string(chunk),
		OutputFrom:      from,
		OutputTruncated: truncated,
	})
}

// handleTaskList serves the daemon's recent tasks, newest first, without their
// output. It is what a client reconciles against when it has lost a task's id.
func (a *App) handleTaskList(c *gin.Context) {
	limit := 20
	if raw := c.Query("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "limit must be a positive number"})
			return
		}
		if value > taskListLimit {
			value = taskListLimit
		}
		limit = value
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "tasks": a.tasks.list(limit)})
}

// handleTaskCancel asks a running task to stop. A task that has already
// finished is not an error: the caller asked for it to stop, and it has. The
// run stops where it is — a compose recreate interrupted this way leaves what
// it had already recreated, which is the honest outcome of stopping it.
func (a *App) handleTaskCancel(c *gin.Context) {
	task, ok := a.tasks.get(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"ok": false, "error": "no such task"})
		return
	}
	task.cancelRun()
	c.JSON(http.StatusOK, gin.H{"ok": true, "task": task.snapshot()})
}
