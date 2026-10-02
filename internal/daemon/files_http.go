package daemon

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// The file API's HTTP surface:
//
//	GET    /api/v1/files/roots
//	GET    /api/v1/files/list?path=
//	GET    /api/v1/files/stat?path=&follow=
//	GET    /api/v1/files/content?path=&offset=&limit=   (raw bytes)
//	POST   /api/v1/files/read                           (JSON, base64 window)
//	POST   /api/v1/files/write                          (JSON, base64 body)
//	PUT    /api/v1/files/content?path=                  (raw body)
//	POST   /api/v1/files/mkdir
//	POST   /api/v1/files/move
//	POST   /api/v1/files/copy
//	POST   /api/v1/files/delete
//
// Authentication happens inside each handler rather than in a middleware,
// because a browser cannot set an Authorization header on the `<a download>`
// and `<img>` requests the file manager uses: GET /api/v1/files/content
// additionally accepts the credential as a `token` query parameter, the same
// accommodation the terminal makes for its WebSocket handshake. No other
// route accepts it, so a URL that leaks into a proxy log or a `Referer` can
// read a file but can never change one. Every mutation presents the
// credential in a header, and a JSON mutation body is additionally bound by
// the usual HMAC signature, so a credential lifted in transit cannot be
// replayed against a body it was never signed for.
//
// Each mount also accepts an alternate verb (see the route table in
// [App.NewApp]) so a client that can only issue one is not locked out of an
// operation.

// fileTokenSubprotocolPrefix carries the file credential in a WebSocket-style
// handshake header. It is deliberately distinct from the terminal's prefix: a
// credential accepted by one API is never implied by the other.
const fileTokenSubprotocolPrefix = "maidcafe.files."

// handleFileRoots reports the configured roots, so a client seeds its browser
// with the directories it may show instead of guessing.
func (a *App) handleFileRoots(c *gin.Context) {
	a.serveFileAction(c, fileActionRoots, nil)
}

func (a *App) handleFileList(c *gin.Context) {
	a.serveFileAction(c, fileActionList, func(req *fileActionRequest) *fileActionError {
		req.Path = strings.TrimSpace(c.Query("path"))
		return nil
	})
}

func (a *App) handleFileStat(c *gin.Context) {
	a.serveFileAction(c, fileActionStat, func(req *fileActionRequest) *fileActionError {
		req.Path = strings.TrimSpace(c.Query("path"))
		if raw := strings.TrimSpace(c.Query("follow")); raw != "" {
			follow, err := strconv.ParseBool(raw)
			if err != nil {
				return fileError(http.StatusBadRequest, "follow must be a boolean")
			}
			req.Follow = &follow
		}
		return nil
	})
}

// handleFileRead serves the JSON/base64 read window the editor and any client
// that cannot issue a raw GET use.
func (a *App) handleFileRead(c *gin.Context) {
	a.serveFileAction(c, fileActionRead, func(req *fileActionRequest) *fileActionError {
		if err := fileQueryWindow(c, req); err != nil {
			return err
		}
		return a.decodeFileBody(c, fileActionRead, req)
	})
}

// handleFileContent streams a read window as raw bytes, so a browser can
// render or download a file without base64 in between. The window is bounded
// by maxReadBytes by construction, and an os.Root-confined file cannot be
// handed to http.ServeContent, so it is buffered before it is written.
func (a *App) handleFileContent(c *gin.Context) {
	started := time.Now()
	policy, policyErr := a.filePolicyFrom()
	if policyErr != nil {
		a.writeFileError(c, policyErr)
		return
	}
	if !fileAuthorizedRequest(c.Request, a.cfg.MetricsSecret, policy.secret, true) {
		a.writeFileError(c, fileError(http.StatusUnauthorized, "unauthorized"))
		return
	}
	target, err := policy.resolve(strings.TrimSpace(c.Query("path")))
	if err != nil {
		a.recordFileOp(fileSourceHTTP, fileActionRead, "", strings.TrimSpace(c.Query("path")), err, started)
		a.writeFileError(c, err)
		return
	}
	offset, err := fileQueryInt(c, "offset", 0)
	if err != nil {
		a.writeFileError(c, err)
		return
	}
	limit, err := fileQueryInt(c, "limit", 0)
	if err != nil {
		a.writeFileError(c, err)
		return
	}
	file, size, window, readErr := policy.openReadWindow(target, offset, limit)
	if readErr != nil {
		a.recordFileOp(fileSourceHTTP, fileActionRead, "", target.path, readErr, started)
		a.writeFileError(c, readErr)
		return
	}
	defer file.Close()
	// The response is exactly the requested window, so it is not an HTTP
	// range reply: the total size travels in a header instead of in
	// Content-Range, and a client that asked for a window knows to keep
	// paging while it has not seen that many bytes.
	c.Header("X-MaidCafe-File-Size", strconv.FormatInt(size, 10))
	if _, err := io.Copy(c.Writer, io.LimitReader(file, window)); err != nil {
		// The status and headers are already committed; the client sees a
		// short body, which its own size check catches.
		return
	}
	a.recordFileOp(fileSourceHTTP, fileActionRead, "", target.path, nil, started)
}

// handleFileWriteRaw accepts the request body itself as the file contents,
// which is how a browser uploads without base64-encoding a large payload.
func (a *App) handleFileWriteRaw(c *gin.Context) {
	policy, policyErr := a.filePolicyFrom()
	if policyErr != nil {
		a.writeFileError(c, policyErr)
		return
	}
	// No query token here: a write must present the credential in a header, so
	// a URL that leaked cannot be replayed into a file change.
	if !fileAuthorizedRequest(c.Request, a.cfg.MetricsSecret, policy.secret, false) {
		a.writeFileError(c, fileError(http.StatusUnauthorized, "unauthorized"))
		return
	}
	started := time.Now()
	rawPath := strings.TrimSpace(c.Query("path"))
	target, resolveErr := policy.resolve(rawPath)
	if resolveErr != nil {
		a.recordFileOp(fileSourceHTTP, fileActionWrite, "", rawPath, resolveErr, started)
		a.writeFileError(c, resolveErr)
		return
	}
	body, readErr := io.ReadAll(io.LimitReader(c.Request.Body, policy.maxWrite+1))
	if readErr != nil {
		a.writeFileError(c, fileError(http.StatusBadRequest, "read request body"))
		return
	}
	if int64(len(body)) > policy.maxWrite {
		a.writeFileError(c, fileError(http.StatusRequestEntityTooLarge, "body exceeds maxWriteBytes (%d)", policy.maxWrite))
		return
	}
	result, actionErr := policy.write(target, body)
	a.recordFileOp(fileSourceHTTP, fileActionWrite, "", target.path, actionErr, started)
	if actionErr != nil {
		a.writeFileError(c, actionErr)
		return
	}
	c.JSON(http.StatusOK, result)
}

// handleFileWrite serves the JSON/base64 write, the counterpart of
// [App.handleFileRead] for clients whose transport is JSON all the way down.
func (a *App) handleFileWrite(c *gin.Context) {
	a.serveFileMutation(c, fileActionWrite)
}

func (a *App) handleFileMkdir(c *gin.Context) {
	a.serveFileMutation(c, fileActionMkdir)
}

func (a *App) handleFileMove(c *gin.Context) {
	a.serveFileMutation(c, fileActionMove)
}

func (a *App) handleFileCopy(c *gin.Context) {
	a.serveFileMutation(c, fileActionCopy)
}

func (a *App) handleFileDelete(c *gin.Context) {
	a.serveFileMutation(c, fileActionDelete)
}

// serveFileMutation runs a mutating action. Parameters arrive in the JSON
// body; anything the body left unset falls back to the query string, so a URL
// built by a form can express the same call.
func (a *App) serveFileMutation(c *gin.Context, action string) {
	a.serveFileAction(c, action, func(req *fileActionRequest) *fileActionError {
		if err := a.decodeFileBody(c, action, req); err != nil {
			return err
		}
		mergeFileQuery(c, req)
		return nil
	})
}

// serveFileAction runs one file action over HTTP: resolve the policy,
// authenticate, build the request with [decorate], execute, audit, answer.
// Authentication comes first so a refusal never reveals whether a path
// exists.
func (a *App) serveFileAction(c *gin.Context, action string, decorate func(*fileActionRequest) *fileActionError) {
	policy, policyErr := a.filePolicyFrom()
	if policyErr != nil {
		a.writeFileError(c, policyErr)
		return
	}
	if !fileAuthorizedRequest(c.Request, a.cfg.MetricsSecret, policy.secret, false) {
		a.writeFileError(c, fileError(http.StatusUnauthorized, "unauthorized"))
		return
	}
	var req fileActionRequest
	if decorate != nil {
		if err := decorate(&req); err != nil {
			a.writeFileError(c, err)
			return
		}
	}
	result, err := a.runFileAction(policy, fileSourceHTTP, action, "", req)
	if err != nil {
		a.writeFileError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// decodeFileBody parses an optional JSON body. A mutation body must carry a
// valid signature, so a credential that leaked into a log cannot be replayed
// to change the body it was attached to. An empty body is allowed: the query
// string may carry everything.
//
// The read cap is base64-inflated: a body of maxWriteBytes of content is
// about a third larger on the wire, plus the JSON envelope, so the limit is
// raised by a fixed margin and the decoded size is then checked exactly by
// the write itself. Capping at the daemon-wide maxBodyBytes instead would
// silently make the JSON route unable to carry the payloads the raw route
// can.
func (a *App) decodeFileBody(c *gin.Context, action string, req *fileActionRequest) *fileActionError {
	limit := a.rt.Load().maxBodyBytes
	if policy := newFilePolicy(a.rt.Load().files); policy != nil {
		if inflated := policy.maxWrite + policy.maxWrite/2 + 64*1024; inflated > limit {
			limit = inflated
		}
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, limit+1))
	if err != nil {
		return fileError(http.StatusBadRequest, "read request body")
	}
	if int64(len(raw)) > limit {
		return fileError(http.StatusRequestEntityTooLarge, "request body too large")
	}
	if len(raw) == 0 {
		return nil
	}
	if !signatureValid(a.cfg.MetricsSecret, raw, c.GetHeader("X-MaidCafe-Signature")) {
		return fileError(http.StatusUnauthorized, "unauthorized")
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		return fileError(http.StatusBadRequest, "invalid JSON body")
	}
	return fileBodyFromValues(values, req, action)
}

// fileBodyFromValues decodes the fields one action consumes. Fields are typed
// strictly: a wrong type is a client bug, not a value to coerce.
func fileBodyFromValues(values map[string]any, req *fileActionRequest, action string) *fileActionError {
	for key, dst := range map[string]*string{
		"path": &req.Path, "from": &req.From, "to": &req.To, "content": &req.Content,
	} {
		if raw, ok := values[key]; ok {
			text, ok := raw.(string)
			if !ok {
				return fileError(http.StatusBadRequest, "%s must be a string", key)
			}
			*dst = text
		}
	}
	for key, dst := range map[string]*bool{
		"parents": &req.Parents, "recursive": &req.Recursive, "overwrite": &req.Overwrite,
	} {
		if raw, ok := values[key]; ok {
			value, ok := raw.(bool)
			if !ok {
				return fileError(http.StatusBadRequest, "%s must be a boolean", key)
			}
			*dst = value
		}
	}
	for key, dst := range map[string]*int64{"offset": &req.Offset, "limit": &req.Limit} {
		if raw, ok := values[key]; ok {
			value, ok := raw.(float64)
			if !ok || value != float64(int64(value)) {
				return fileError(http.StatusBadRequest, "%s must be an integer", key)
			}
			if value < 0 {
				return fileError(http.StatusBadRequest, "%s must not be negative", key)
			}
			*dst = int64(value)
		}
	}
	if raw, ok := values["follow"]; ok {
		value, ok := raw.(bool)
		if !ok {
			return fileError(http.StatusBadRequest, "follow must be a boolean")
		}
		req.Follow = &value
	}
	return nil
}

// fileQueryWindow reads an optional read window from the query string.
func fileQueryWindow(c *gin.Context, req *fileActionRequest) *fileActionError {
	req.Path = strings.TrimSpace(c.Query("path"))
	offset, err := fileQueryInt(c, "offset", 0)
	if err != nil {
		return err
	}
	limit, err := fileQueryInt(c, "limit", 0)
	if err != nil {
		return err
	}
	req.Offset, req.Limit = offset, limit
	return nil
}

// mergeFileQuery fills fields the JSON body left unset from the query string.
func mergeFileQuery(c *gin.Context, req *fileActionRequest) {
	if req.Path == "" {
		req.Path = strings.TrimSpace(c.Query("path"))
	}
	if req.From == "" {
		req.From = strings.TrimSpace(c.Query("from"))
	}
	if req.To == "" {
		req.To = strings.TrimSpace(c.Query("to"))
	}
	queryBool(c, "recursive", &req.Recursive)
	queryBool(c, "parents", &req.Parents)
	queryBool(c, "overwrite", &req.Overwrite)
}

func queryBool(c *gin.Context, name string, dst *bool) {
	if *dst {
		return
	}
	if raw := strings.TrimSpace(c.Query(name)); raw != "" {
		if value, err := strconv.ParseBool(raw); err == nil {
			*dst = value
		}
	}
}

// fileAuthorizedRequest reports whether the request carries the file
// credential in one of the two header-borne places a client can put it — the
// Bearer header or a `maidcafe.files.<token>` subprotocol token — plus, when
// [allowQueryToken] is set, the `token` query parameter.
//
// The query parameter exists for the one request a browser cannot put a
// header on: a download or an image load against the raw-content route. It is
// deliberately not accepted anywhere else, so a URL that leaks into a proxy
// log or a `Referer` can read a file but can never write, move or delete one.
// Equality is constant-time, because a credential compared byte-by-byte leaks
// its prefix to a caller who can time the failures.
func fileAuthorizedRequest(r *http.Request, metricsSecret, fileSecret string, allowQueryToken bool) bool {
	expected := fileExpectedSecret(fileSecret, metricsSecret)
	if expected == "" {
		return false
	}
	if secret, ok := bearerSecret(r); ok && constantTimeEqual(secret, expected) {
		return true
	}
	if secret, ok := fileSubprotocolSecret(r.Header.Get("Sec-WebSocket-Protocol")); ok && constantTimeEqual(secret, expected) {
		return true
	}
	if allowQueryToken {
		if secret := strings.TrimSpace(r.URL.Query().Get("token")); secret != "" && constantTimeEqual(secret, expected) {
			return true
		}
	}
	return false
}

// fileExpectedSecret resolves the accepted file credential: the dedicated
// file secret when configured, the metrics secret otherwise. It mirrors
// terminalExpectedSecret, so the two APIs have one rule for what "no separate
// secret configured" means.
func fileExpectedSecret(fileSecret, metricsSecret string) string {
	if fileSecret != "" {
		return fileSecret
	}
	return metricsSecret
}

// fileSubprotocolSecret decodes the first `maidcafe.files.<base64url>` token
// of a Sec-WebSocket-Protocol header. The encoding is unpadded base64url,
// since a subprotocol token cannot contain "=".
func fileSubprotocolSecret(header string) (string, bool) {
	for _, token := range strings.Split(header, ",") {
		token = strings.TrimSpace(token)
		if !strings.HasPrefix(token, fileTokenSubprotocolPrefix) {
			continue
		}
		encoded := strings.TrimPrefix(token, fileTokenSubprotocolPrefix)
		decoded, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil || len(decoded) == 0 {
			return "", false
		}
		return string(decoded), true
	}
	return "", false
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// writeFileError maps a file action failure onto the HTTP response.
func (a *App) writeFileError(c *gin.Context, err *fileActionError) {
	c.JSON(err.status, gin.H{"ok": false, "error": err.message})
}

// fileQueryInt parses an optional non-negative integer query parameter.
func fileQueryInt(c *gin.Context, name string, fallback int64) (int64, *fileActionError) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, fileError(http.StatusBadRequest, "%s must be a non-negative integer", name)
	}
	return value, nil
}
