// Package webui serves the built console (internal/cloud/web/dist) from the
// cloud HTTP server. Files that exist in dist are served by a middleware
// mounted before the API routes, so a fingerprinted asset never costs an auth
// round-trip; unknown extension-less paths fall back to index.html from the
// NoRoute handler so client-side routes survive a reload.
package webui

import (
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
)

// reservedPrefixes are served by the service itself and must never fall back
// to the SPA. Matching is segment-based, so /apiary is not reserved.
var reservedPrefixes = []string{"/api", "/health", "/alive", "/ready", "/favicon.png", "/favicon.ico"}

// Handler serves the SPA from a dist directory. The zero value (and a handler
// built with an empty dist path) is disabled.
type Handler struct {
	root fs.FS
}

// New opens the built SPA at dist. An empty dist returns a disabled handler; a
// missing directory or a directory without index.html returns an error and a
// disabled handler.
func New(dist string) (*Handler, error) {
	dist = strings.TrimSpace(dist)
	if dist == "" {
		return &Handler{}, nil
	}
	info, err := os.Stat(dist)
	if err != nil {
		return &Handler{}, err
	}
	if !info.IsDir() {
		return &Handler{}, &fs.PathError{Op: "open", Path: dist, Err: fs.ErrInvalid}
	}
	root := os.DirFS(dist)
	if _, err := fs.Stat(root, "index.html"); err != nil {
		return &Handler{}, err
	}
	return &Handler{root: root}, nil
}

// Enabled reports whether a dist directory was mounted.
func (h *Handler) Enabled() bool {
	return h != nil && h.root != nil
}

// ServeStatic is a gin middleware mounted before the API routes: a GET or HEAD
// for an existing file in dist is served here and never reaches the route
// tree; every other request continues down the chain.
func (h *Handler) ServeStatic(c *gin.Context) {
	if !h.Enabled() || (c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead) {
		c.Next()
		return
	}
	name, ok := h.file(c)
	if !ok {
		c.Next()
		return
	}
	if h.serveFile(c, name) {
		c.Abort()
		return
	}
	c.Next()
}

// Serve is the gin NoRoute handler: unknown extension-less paths get
// index.html (client-side routing), missing assets and reserved paths get the
// canonical JSON 404.
func (h *Handler) Serve(c *gin.Context) {
	if !h.Enabled() || isReserved(c.Request.URL.Path) || hasDotSegment(c.Request.URL.Path) {
		notFound(c)
		return
	}
	if path.Ext(c.Request.URL.Path) != "" {
		notFound(c)
		return
	}
	h.serveIndex(c)
}

func (h *Handler) file(c *gin.Context) (string, bool) {
	requestPath := c.Request.URL.Path
	if requestPath == "" || requestPath == "/" || hasDotSegment(requestPath) || isReserved(requestPath) {
		return "", false
	}
	name := strings.TrimPrefix(path.Clean(requestPath), "/")
	if name == "" || name == "." {
		return "", false
	}
	info, err := fs.Stat(h.root, name)
	if err != nil || info.IsDir() {
		return "", false
	}
	return name, true
}
func (h *Handler) serveFile(c *gin.Context, name string) bool {
	file, err := h.root.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return false
	}
	if strings.HasPrefix(c.Request.URL.Path, "/assets/") {
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		c.Header("Cache-Control", "public, max-age=3600")
	}
	http.ServeContent(c.Writer, c.Request, name, info.ModTime(), file.(io.ReadSeeker))
	return true
}

func (h *Handler) serveIndex(c *gin.Context) {
	index, err := fs.ReadFile(h.root, "index.html")
	if err != nil {
		notFound(c)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/html; charset=utf-8", index)
}

func isReserved(p string) bool {
	cleaned := path.Clean("/" + p)
	for _, prefix := range reservedPrefixes {
		if cleaned == prefix || strings.HasPrefix(cleaned, prefix+"/") {
			return true
		}
	}
	return false
}

func hasDotSegment(p string) bool {
	for _, segment := range strings.Split(p, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func notFound(c *gin.Context) {
	c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
}
