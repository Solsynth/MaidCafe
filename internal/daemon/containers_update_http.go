package daemon

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Update endpoints.
//
// These are a separate concern from the detail reads: they compare a container
// against a third party rather than reading the host, so they are the only
// daemon endpoints that make an outbound request on a client's behalf. The
// comparison itself lives in updatecheck.go (and the registry reads in
// registry.go); these handlers only resolve the container and cache the answer.

// handleContainerUpdateCheck serves one container's update status, checking it
// now when the daemon has no recent answer. The refresh floor is what keeps a
// client from turning this endpoint into a registry load generator.
func (a *App) handleContainerUpdateCheck(c *gin.Context) {
	ref, found, err := a.resolveContainer(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}
	if !found {
		notFoundContainer(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"container": a.updateCheck.Refresh(c.Request.Context(), ref, updateCheckRefreshFloor),
	})
}

// handleContainerUpdates serves every cached update status without touching a
// registry, so a client can paint badges from one cheap request and then ask
// for individual containers to be checked.
func (a *App) handleContainerUpdates(c *gin.Context) {
	interval := int64(0)
	if rt := a.rt.Load(); rt != nil {
		interval = int64(rt.intervals.updateCheck.Seconds())
	}
	c.JSON(http.StatusOK, containerUpdatesPayload{
		IntervalSeconds: interval,
		Containers:      a.updateCheck.Results(),
	})
}
