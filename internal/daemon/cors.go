package daemon

import (
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// What a preflight is answered with.
//
// The method list is this daemon's whole HTTP surface, and the header list is
// what its clients send: the credential, a JSON body's content type, and the
// two headers a signed action carries. A client that asks for anything else has
// its own requested headers echoed back, so a new client header does not need a
// daemon release to work — the origin was already trusted, and anything it was
// not trusted to send is refused by the browser either way.
const (
	corsAllowedMethods = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	corsAllowedHeaders = "Authorization, Content-Type"
	// A browser can read only these response headers of a cross-origin reply.
	// Content-Type and Content-Length are readable without being listed.
	corsExposedHeaders = "X-MaidCafe-File-Size"
	// Seconds a browser may cache a preflight answer.
	corsMaxAge = "600"
)

// corsMiddleware answers the browser's cross-origin rules for the origins this
// daemon serves.
//
// A browser cannot open a raw SSH socket, so it reaches a host through this
// daemon — but the browser also refuses to let a page *read* an answer from a
// server that did not name it, which is every browser read of metrics,
// container state, files and the event stream. The terminal is a WebSocket and
// is not policed this way, so a daemon whose terminal works can still look
// unreachable to a browser build everywhere else.
//
// The allowlist is the terminal one (`daemon.terminal.allowedOrigins`). An
// origin listed there is already granted an interactive shell on this host,
// which is strictly more than the control plane offers, and one list means an
// operator who has already named the web build does not have to name it twice.
//
// Two rules are deliberate:
//
//   - No credentials are echoed. Every credential this API takes travels in an
//     Authorization header, so `Access-Control-Allow-Credentials` stays off and
//     a page cannot ride a session the browser would attach by itself.
//   - An origin that is not listed gets no headers rather than an error. The
//     browser blocks the read, which is the intended answer, and a non-browser
//     client — which sends no Origin at all — is unaffected.
//
// The policy is read per request, so a config reload takes effect without a
// restart.
func corsMiddleware(policy func() config.TerminalConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := strings.TrimSpace(c.GetHeader("Origin"))
		if origin == "" {
			// Not a cross-origin browser request: native clients, tunnels and
			// same-origin pages need nothing from here.
			c.Next()
			return
		}
		// The answer depends on the caller's Origin, allowed or not, so a shared
		// cache must never reuse one caller's response for another.
		c.Writer.Header().Add("Vary", "Origin")
		if !originAllowed(policy().AllowedOrigins, origin, c.Request.Host) {
			c.Next()
			return
		}
		header := c.Writer.Header()
		// The caller's own origin rather than "*": it is the answer to a question
		// only this caller asked, and it is what lets the response stay
		// credential-free.
		header.Set("Access-Control-Allow-Origin", origin)
		if c.Request.Method == http.MethodOptions {
			// A preflight never reaches a handler: it asks only whether the real
			// request may be made.
			header.Set("Access-Control-Allow-Methods", corsAllowedMethods)
			header.Set("Access-Control-Allow-Headers", corsAllowHeaders(c.Request))
			header.Set("Access-Control-Max-Age", corsMaxAge)
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		header.Set("Access-Control-Expose-Headers", corsExposedHeaders)
		c.Next()
	}
}

// corsAllowHeaders names the headers a preflight may be told about: the
// caller's own request when it asked for any, so a client header this build of
// the daemon predates still works, and otherwise the ones this daemon's clients
// send.
func corsAllowHeaders(r *http.Request) string {
	if requested := strings.TrimSpace(r.Header.Get("Access-Control-Request-Headers")); requested != "" {
		return requested
	}
	return corsAllowedHeaders
}

// originAllowed reports whether [origin] may read this daemon's answers.
//
// The rule is the terminal's, deliberately: an origin whose host equals the
// request's own Host is always allowed — that is a page the daemon itself is
// serving, or a reverse proxy in front of it — and otherwise each configured
// pattern is matched with `path.Match`, against the origin's host, or against
// `scheme://host` when the pattern names a scheme.
func originAllowed(patterns []string, origin, requestHost string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	if strings.EqualFold(requestHost, parsed.Host) {
		return true
	}
	for _, pattern := range patterns {
		target := parsed.Host
		if strings.Contains(pattern, "://") {
			target = parsed.Scheme + "://" + parsed.Host
		}
		matched, err := path.Match(pattern, target)
		if err == nil && matched {
			return true
		}
	}
	return false
}
