package handler

import (
	"errors"
	"math"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
	dyauth "src.solsynth.dev/sosys/lighthouse/shared/pkg/auth"
	"src.solsynth.dev/sosys/lighthouse/shared/pkg/handoff"
)

const (
	authTokenCookieName    = "AuthToken"
	refreshTokenCookieName = "RefreshToken"
)

// AccountView is the console's identity: enough to draw the header and decide
// which workspace surfaces to offer, and nothing that would leak a secret.
type AccountView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Nick        string `json:"nick"`
	IsSuperuser bool   `json:"is_superuser"`
	Picture     string `json:"picture,omitempty"`
}

type SessionView struct {
	Account   AccountView `json:"account"`
	ExpiresIn int64       `json:"expires_in,omitempty"`
}

type handoffRoutes struct {
	cfg           *config.Config
	authenticator dyauth.TokenAuthenticator
	client        *handoff.TokenClient
}

// RegisterAuthRoutes mounts the browser-facing half of the fleet session
// hand-off. It sits outside the user group because a browser arrives here
// with a code, not a token: the exchange is what creates its session cookie.
func RegisterAuthRoutes(r *gin.Engine, cfg *config.Config, authenticator dyauth.TokenAuthenticator) {
	routes := &handoffRoutes{cfg: cfg, authenticator: authenticator}
	if cfg != nil && cfg.HandoffEnabled() {
		routes.client = &handoff.TokenClient{
			BaseURL:      strings.TrimSpace(cfg.Handoff.BaseURL),
			ClientID:     strings.TrimSpace(cfg.Handoff.ClientID),
			ClientSecret: cfg.Handoff.ClientSecret,
		}
	}
	r.GET("/api/auth/config", routes.config)
	r.POST("/api/auth/session", routes.redeem)
	r.POST("/api/auth/refresh", routes.refresh)
	r.POST("/api/auth/logout", routes.logout)
}

func (h *handoffRoutes) enabled() bool {
	return h != nil && h.cfg != nil && h.cfg.HandoffEnabled() && h.client != nil
}

// config publishes the issuer's sign-in page. The browser needs the host
// before it has a session, and the deployment already owns that value in
// [handoff] baseUrl; the console assumes no identity host of its own.
func (h *handoffRoutes) config(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !h.enabled() {
		c.JSON(http.StatusOK, gin.H{"configured": false, "sign_in_url": ""})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"configured":  true,
		"sign_in_url": strings.TrimRight(strings.TrimSpace(h.client.BaseURL), "/") + "/sign-in",
	})
}

func (h *handoffRoutes) redeem(c *gin.Context) {
	if !h.enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "session hand-off is not configured"})
		return
	}
	var input struct {
		Code string `json:"code"`
	}
	if err := c.ShouldBindJSON(&input); err != nil || strings.TrimSpace(input.Code) == "" {
		writeHandoffFailure(c, handoff.NewError(handoff.CodeInvalidRequest, "The request carried no hand-off code."))
		return
	}
	tokens, err := h.client.Redeem(c.Request.Context(), input.Code)
	if err != nil {
		writeHandoffFailure(c, err)
		return
	}
	// The code is single-use, so the token must be verified before the session
	// is accepted: an unverifiable token would consume the code and leave the
	// browser with cookies no API can authenticate.
	account, err := h.verify(c, tokens.Token)
	if err != nil {
		writeHandoffFailure(c, err)
		return
	}
	h.setSessionCookies(c, tokens)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, SessionView{Account: account, ExpiresIn: tokens.ExpiresIn})
}

func (h *handoffRoutes) refresh(c *gin.Context) {
	if !h.enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "session hand-off is not configured"})
		return
	}
	refreshToken, err := c.Cookie(refreshTokenCookieName)
	if err != nil || strings.TrimSpace(refreshToken) == "" {
		h.clearSessionCookies(c)
		writeInvalidGrant(c)
		return
	}
	tokens, err := h.client.Renew(c.Request.Context(), refreshToken)
	if err != nil {
		h.clearSessionCookies(c)
		writeInvalidGrant(c)
		return
	}
	account, err := h.verify(c, tokens.Token)
	if err != nil {
		h.clearSessionCookies(c)
		writeHandoffFailure(c, err)
		return
	}
	h.setSessionCookies(c, tokens)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, SessionView{Account: account, ExpiresIn: tokens.ExpiresIn})
}

func (h *handoffRoutes) logout(c *gin.Context) {
	h.clearSessionCookies(c)
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusOK)
}

// verify asks the auth service whether the freshly issued token is usable.
// A nil authenticator is a deployment error, not an invalid grant.
func (h *handoffRoutes) verify(c *gin.Context, token string) (AccountView, error) {
	if h.authenticator == nil {
		return AccountView{}, handoff.NewError(handoff.CodeServerError, "The cloud has no identity connection to verify the handed-off session.")
	}
	result, err := h.authenticator.Authenticate(c.Request.Context(), dyauth.TokenInfo{
		Token: token,
		Type:  dyauth.TokenTypeAuthKey,
	}, c.Request)
	if err != nil {
		return AccountView{}, handoff.NewError(handoff.CodeServerError, "The cloud could not verify the handed-off session.")
	}
	if result == nil || result.Account == nil {
		return AccountView{}, handoff.NewError(handoff.CodeServerError, "The identity service returned no account for the handed-off session.")
	}
	return viewAccount(result), nil
}

// currentSession is the authenticated read: the console uses it on boot to
// learn who the cookie belongs to, without another call to the issuer.
func currentSession(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	result, _, ok := dyauth.GetAuth(c)
	if !ok || result == nil || result.Account == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	c.JSON(http.StatusOK, SessionView{Account: viewAccount(result)})
}

func viewAccount(result *dyauth.AuthResult) AccountView {
	account := result.Account
	view := AccountView{
		ID:          account.GetId(),
		Name:        account.GetName(),
		Nick:        account.GetNick(),
		IsSuperuser: account.GetIsSuperuser(),
	}
	if picture := account.GetPicture(); picture != nil {
		view.Picture = picture.GetUrl()
	}
	return view
}

func (h *handoffRoutes) setSessionCookies(c *gin.Context, tokens *handoff.RedeemWireResponse) {
	c.SetSameSite(http.SameSiteLaxMode)
	h.setCookie(c, authTokenCookieName, tokens.Token, cookieMaxAge(tokens.ExpiresIn))
	if refresh := strings.TrimSpace(tokens.RefreshToken); refresh != "" {
		h.setCookie(c, refreshTokenCookieName, refresh, cookieMaxAge(tokens.RefreshExpiresIn))
	} else {
		h.setCookie(c, refreshTokenCookieName, "", -1)
	}
}

func (h *handoffRoutes) clearSessionCookies(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	h.setCookie(c, authTokenCookieName, "", -1)
	h.setCookie(c, refreshTokenCookieName, "", -1)
}

func (h *handoffRoutes) setCookie(c *gin.Context, name, value string, maxAge int) {
	domain := ""
	secure := false
	if h.cfg != nil {
		domain = strings.TrimSpace(h.cfg.Handoff.CookieDomain)
		secure = h.cfg.Handoff.CookieSecure
	}
	c.SetCookie(name, value, maxAge, "/", domain, secure, true)
}

func cookieMaxAge(seconds int64) int {
	if seconds <= 0 {
		return 0
	}
	if seconds > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(seconds)
}

func writeInvalidGrant(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusUnauthorized, gin.H{"error": handoff.CodeInvalidGrant})
}

func writeHandoffFailure(c *gin.Context, err error) {
	body := gin.H{"error": handoff.ErrorCodeFor(err)}
	var protocol *handoff.Error
	if errors.As(err, &protocol) && strings.TrimSpace(protocol.Message) != "" {
		body["error_description"] = protocol.Message
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(handoff.HTTPStatusForError(err), body)
}
