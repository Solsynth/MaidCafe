package daemon

// Managed compose stack endpoints.
//
// These are the operator's side of the registry the daemon keeps: what a scan
// found, and what it was asked to look for. A stack is acted on through the
// native operations (`container.update`, `compose.update`, the compose verbs),
// which resolve their directory here or from a container's own labels.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// composeScanInput is a scan request's body: where to look, and how deep.
type composeScanInput struct {
	// Path is one starting point: the directory a scan begins at.
	Path string `json:"path"`
	// Roots is a set of starting points. Sending both is refused rather than
	// merged, because the two answer different questions ("look there" versus
	// "look in these places").
	Roots []string `json:"roots"`
	// Depth overrides the configured scan depth for this request.
	Depth int `json:"depth"`
}

// composeStackContainer is one container a managed stack is running.
type composeStackContainer struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	State   string `json:"state"`
	Runtime string `json:"runtime"`
}

// composeStackView is a managed stack plus what the daemon currently sees of it:
// the health watch the registry exists for.
type composeStackView struct {
	composeStack
	Running    int                     `json:"running"`
	Total      int                     `json:"total"`
	Containers []composeStackContainer `json:"containers"`
}

// composeScanView is where a scan would look, so a client can show the operator
// what a scan will do before it does it.
type composeScanView struct {
	Roots    []string `json:"roots"`
	Depth    int      `json:"depth"`
	MaxFiles int      `json:"max_files"`
}

// handleComposeStacks serves the managed stacks and the scan policy.
func (a *App) handleComposeStacks(c *gin.Context) {
	stacks, err := a.composeStackViews(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":     true,
		"stacks": stacks,
		"scan":   a.composeScanView(),
	})
}

// handleComposeStacksScan runs a scan and assigns what it finds. The body is
// signed like every other mutating request: it decides which directories the
// daemon henceforth runs compose commands in.
func (a *App) handleComposeStacksScan(c *gin.Context) {
	input, ok := a.signedScanInput(c)
	if !ok {
		return
	}
	roots, err := a.composeScanRoots(input)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error()})
		return
	}
	limits := a.composeScanLimits(input.Depth)
	ctx, cancel := context.WithTimeout(c.Request.Context(), composeScanTimeout)
	defer cancel()
	found := scanComposeStacks(ctx, roots, limits)
	added, updated, removed := a.composeStacks.Apply(found)
	a.logger.Info("compose scan completed",
		"roots", len(roots), "found", len(found),
		"added", len(added), "updated", len(updated), "removed", len(removed))
	stacks, err := a.composeStackViews(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"roots":   roots,
		"found":   len(found),
		"added":   stackNames(added),
		"updated": stackNames(updated),
		"removed": stackNames(removed),
		"stacks":  stacks,
	})
}

// handleComposeStackRemove forgets one managed stack, leaving its containers
// and files alone: unassigning is a statement about this daemon, not about the
// host.
func (a *App) handleComposeStackRemove(c *gin.Context) {
	project := strings.TrimSpace(c.Param("project"))
	if project == "" {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "a project name is required"})
		return
	}
	stack, removed := a.composeStacks.Remove(project)
	if !removed {
		c.JSON(http.StatusNotFound, gin.H{"ok": false, "error": "no such managed stack"})
		return
	}
	a.logger.Info("compose stack unassigned", "project", stack.Project, "directory", stack.Directory)
	c.JSON(http.StatusOK, gin.H{"ok": true, "stack": stack})
}

// composeStackViews joins the registry with the daemon's own container
// snapshot, so one request answers both "what is assigned" and "what is it
// running".
func (a *App) composeStackViews(ctx context.Context) ([]composeStackView, error) {
	stacks := a.composeStacks.List()
	views := make([]composeStackView, 0, len(stacks))
	for _, stack := range stacks {
		views = append(views, composeStackView{composeStack: stack, Containers: []composeStackContainer{}})
	}
	if len(views) == 0 {
		return views, nil
	}
	data, err := a.containers.snapshot(ctx)
	if err != nil {
		// The registry is still worth serving without the container snapshot:
		// an unavailable runtime is not a missing assignment.
		return views, nil
	}
	var payload containersPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return views, nil
	}
	byProject := make(map[string][]composeStackContainer)
	for _, runtime := range payload.Runtimes {
		for _, container := range runtime.Containers {
			project := strings.TrimSpace(container.ComposeProject)
			if project == "" {
				continue
			}
			key := strings.ToLower(project)
			byProject[key] = append(byProject[key], composeStackContainer{
				ID:      container.ID,
				Name:    container.Name,
				Image:   container.Image,
				State:   container.State,
				Runtime: runtime.Runtime,
			})
		}
	}
	for i := range views {
		containers := byProject[views[i].key()]
		sort.Slice(containers, func(left, right int) bool {
			return containers[left].Name < containers[right].Name
		})
		views[i].Containers = containers
		views[i].Total = len(containers)
		for _, container := range containers {
			if container.State == "running" {
				views[i].Running++
			}
		}
	}
	return views, nil
}

// composeScanView reports where a scan would look when a request names nothing.
func (a *App) composeScanView() composeScanView {
	limits := a.composeScanLimits(0)
	return composeScanView{
		Roots:    a.composeScanDefaults(),
		Depth:    limits.depth,
		MaxFiles: limits.maxFiles,
	}
}

// composeScanLimits is the configured scan depth, or the request's when it sent
// one. The depth is clamped rather than refused so a client cannot ask for an
// unbounded walk by accident.
func (a *App) composeScanLimits(requested int) composeScanLimits {
	limits := composeScanLimitsDefault
	if rt := a.rt.Load(); rt != nil && rt.compose.ScanDepth > 0 {
		limits.depth = rt.compose.ScanDepth
	}
	if requested > 0 {
		limits.depth = requested
	}
	if limits.depth > 12 {
		limits.depth = 12
	}
	return limits
}

// composeScanDefaults is the configured root list, or the built-in one.
func (a *App) composeScanDefaults() []string {
	roots := a.composeConfiguredRoots()
	if len(roots) == 0 {
		return composeDefaultScanRoots
	}
	return roots
}

func (a *App) composeConfiguredRoots() []string {
	rt := a.rt.Load()
	if rt == nil || len(rt.compose.ScanRoots) == 0 {
		return nil
	}
	return rt.compose.ScanRoots
}

// composeScanRoots is what one request scans: its starting point, its roots, or
// the configured defaults.
func (a *App) composeScanRoots(input composeScanInput) ([]string, error) {
	switch {
	case strings.TrimSpace(input.Path) != "" && len(input.Roots) > 0:
		return nil, fmt.Errorf("send either a path or roots, not both")
	case strings.TrimSpace(input.Path) != "":
		path, err := validateComposeScanPath(input.Path)
		if err != nil {
			return nil, err
		}
		return []string{path}, nil
	case len(input.Roots) > 0:
		if len(input.Roots) > composeScanMaxRoots {
			return nil, fmt.Errorf("at most %d roots per scan", composeScanMaxRoots)
		}
		roots := make([]string, 0, len(input.Roots))
		for _, root := range input.Roots {
			cleaned, err := validateComposeScanPath(root)
			if err != nil {
				return nil, err
			}
			roots = append(roots, cleaned)
		}
		return roots, nil
	default:
		return a.composeScanDefaults(), nil
	}
}

// signedScanInput reads and authorizes a scan request's body. The signature is
// the same HMAC-over-body every native operation carries, because this request
// answers the same question those do: which directories the daemon acts in.
func (a *App) signedScanInput(c *gin.Context) (composeScanInput, bool) {
	var input composeScanInput
	maxBody := a.rt.Load().maxBodyBytes
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBody+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error()})
		return input, false
	}
	if int64(len(body)) > maxBody {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"ok": false, "error": "request body too large"})
		return input, false
	}
	if !signatureValid(a.cfg.MetricsSecret, body, c.GetHeader("X-MaidCafe-Signature")) {
		c.JSON(http.StatusUnauthorized, gin.H{"ok": false, "error": "unauthorized"})
		return input, false
	}
	if len(body) == 0 {
		return input, true
	}
	if err := json.Unmarshal(body, &input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "invalid JSON body"})
		return input, false
	}
	return input, true
}

func stackNames(stacks []composeStack) []string {
	names := make([]string, 0, len(stacks))
	for _, stack := range stacks {
		names = append(names, stack.Project)
	}
	sort.Strings(names)
	return names
}

// composeScanMaxRoots caps how many starting points one request may name.
const composeScanMaxRoots = 32

// composeScanTimeout bounds a scan's filesystem walk. Every bound a scan
// applies is about work, not trust: a scan reads directories the operator
// named, and stops at the depth and file count it was configured with.
const composeScanTimeout = 30 * time.Second
