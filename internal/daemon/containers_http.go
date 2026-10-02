package daemon

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// Container detail endpoints.
//
// The list endpoint (`GET /api/v1/containers`) is the cheap, cacheable view the
// stream pushes too. These are the per-container reads behind it: what a
// container was created from, what it is doing now, and what it logged. All of
// them resolve their container through the daemon's own container snapshot, so
// a request names a container the same way everywhere — id, id prefix or name —
// and an unknown one is a clean 404.

// notFoundContainer answers a request for a container the daemon cannot see.
// It is deliberately the same answer for an unknown id and for an id on a
// runtime that is not installed: neither is reachable from this daemon.
func notFoundContainer(c *gin.Context) {
	c.JSON(http.StatusNotFound, gin.H{"ok": false, "error": "no such container"})
}

// containerReadFailure answers a runtime read that failed, with the runtime's
// own message: "no such container" or "permission denied" tells an operator
// what to do, where "bad gateway" does not.
func containerReadFailure(c *gin.Context, err error) {
	c.JSON(http.StatusBadGateway, gin.H{"ok": false, "error": err.Error()})
}

// handleContainerInspect serves one container's inspect payload: the runtime's
// own view of how the container was created — image, command, environment,
// mounts, ports, networks, labels, restart policy. It is passed through
// unmodified, because the point of inspect is completeness; note that it
// includes the container's environment, so it is only ever served to a caller
// that holds the daemon's credentials.
func (a *App) handleContainerInspect(c *gin.Context) {
	ref, found, err := a.resolveContainer(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}
	if !found {
		notFoundContainer(c)
		return
	}
	info, err := inspectContainer(c.Request.Context(), ref.Path, ref.ID)
	if err != nil {
		containerReadFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"container": ref.ID,
		"name":      ref.Name,
		"runtime":   ref.Runtime,
		"inspect":   json.RawMessage(info.JSON),
	})
}

// handleContainerStats serves one container's current resource usage.
func (a *App) handleContainerStats(c *gin.Context) {
	ref, found, err := a.resolveContainer(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}
	if !found {
		notFoundContainer(c)
		return
	}
	stats, err := containerStatsFor(c.Request.Context(), ref)
	if err != nil {
		containerReadFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, stats)
}

// handleContainerLogs serves one container's log tail from either source:
// `captured` (the default) is the daemon's own disk-backed tail, which is the
// history that survives a container restart; `runtime` asks the runtime
// directly, which answers for a container the daemon never tailed — one whose
// logs interval is disabled, or one that started a moment ago.
func (a *App) handleContainerLogs(c *gin.Context) {
	source := strings.ToLower(strings.TrimSpace(c.Query("source")))
	switch source {
	case "":
		source = "captured"
	case "captured", "runtime":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "source must be captured or runtime"})
		return
	}
	lines := 200
	if raw := strings.TrimSpace(c.Query("lines")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1 || parsed > containerLogRingLines {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "lines must be between 1 and " + strconv.Itoa(containerLogRingLines)})
			return
		}
		lines = parsed
	}
	if source == "captured" {
		c.JSON(http.StatusOK, gin.H{
			"container": c.Param("id"),
			"source":    source,
			"lines":     a.logs.store.Snapshot(c.Param("id"), lines),
		})
		return
	}
	ref, found, err := a.resolveContainer(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}
	if !found {
		notFoundContainer(c)
		return
	}
	tail, err := runtimeLogs(c.Request.Context(), ref.Path, ref.ID, lines)
	if err != nil {
		containerReadFailure(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"container": ref.ID, "source": source, "lines": tail})
}
