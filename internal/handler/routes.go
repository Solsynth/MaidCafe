package handler

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"src.solsynth.dev/solsynth/maidcafe/internal/cloud"
	"src.solsynth.dev/solsynth/maidcafe/internal/database"
	dyauth "src.solsynth.dev/sosys/go/pkg/auth"
)

func RegisterRoutes(r *gin.Engine, svc *cloud.Service, userAuth gin.HandlerFunc) {
	r.GET("/", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(cloudLandingPageHTML))
	})
	r.GET("/favicon.png", serveFavicon)
	r.GET("/favicon.ico", serveFavicon)
	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true, "mode": "cloud"}) })
	user := r.Group("/api")
	user.Use(userAuth, requireUser())
	user.POST("/daemons", createDaemon(svc))
	user.GET("/daemons", listDaemons(svc))
	user.GET("/daemons/:id/metrics", listMetrics(svc))
	user.POST("/daemons/:id/push-notification", requestPushNotification(svc))
	user.GET("/daemons/:id", getDaemon(svc))
	user.PATCH("/daemons/:id", updateDaemon(svc))
	user.POST("/daemons/:id/rotate-secret", rotateSecret(svc))
	user.DELETE("/daemons/:id", disableDaemon(svc))
	user.GET("/notifications", listNotifications(svc))
	user.POST("/notifications/:id/read", markRead(svc))
	user.POST("/daemons/:id/webhook-requests", enqueueWebhook(svc))
	user.GET("/daemons/:id/webhook-requests/:request_id", getWebhookResult(svc))
	user.GET("/daemons/:id/actions", listActions(svc))
	user.POST("/credentials", createCredential(svc))
	user.GET("/credentials", listCredentials(svc))
	user.DELETE("/credentials/:id", deleteCredential(svc))
	daemon := r.Group("/api/daemons/:id")
	daemon.POST("/metrics", ingestMetric(svc))
	daemon.POST("/actions", syncActions(svc))
	daemon.POST("/notifications", createNotification(svc))
	daemon.GET("/webhook-requests/pending", listPendingWebhooks(svc))
	daemon.POST("/webhook-requests/:request_id/result", completeWebhook(svc))
}

func requireUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, _, ok := dyauth.GetAuth(c); !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}
func accountID(c *gin.Context) string {
	result, _, _ := dyauth.GetAuth(c)
	return result.Account.GetId()
}

func userHandle(c *gin.Context) string {
	result, _, _ := dyauth.GetAuth(c)
	if result == nil || result.Account == nil {
		return ""
	}
	if nick := result.Account.GetNick(); nick != "" {
		return nick
	}
	if name := result.Account.GetName(); name != "" {
		return name
	}
	return result.Account.GetId()
}

// credentialContext stores the authenticated API credential on the request
// so handlers can enforce its scopes; absent means a Solarpass user.
const credentialContextKey = "maidcafe_credential"

func WithCredential(c *gin.Context, credential *database.Credential) {
	c.Set(credentialContextKey, credential)
}

func credentialFrom(c *gin.Context) *database.Credential {
	if raw, ok := c.Get(credentialContextKey); ok {
		if credential, ok := raw.(*database.Credential); ok {
			return credential
		}
	}
	return nil
}

func createCredential(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var in struct {
			Label       string   `json:"label"`
			DaemonIDs   []string `json:"daemon_ids"`
			HostIDs     []string `json:"host_ids"`
			ActionNames []string `json:"action_names"`
		}
		if !parseJSON(c, &in) {
			return
		}
		out, err := s.CreateCredential(c, accountID(c), in.Label, in.DaemonIDs, in.HostIDs, in.ActionNames)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusCreated, out)
	}
}

func listCredentials(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		out, err := s.ListCredentials(c, accountID(c))
		if err != nil {
			serviceStatus(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

func deleteCredential(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := s.DeleteCredential(c, accountID(c), c.Param("id")); err != nil {
			serviceStatus(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}
func parseJSON(c *gin.Context, dst any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid JSON"})
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid JSON"})
		return false
	}
	return true
}
func serviceStatus(c *gin.Context, err error) {
	switch {
	case errors.Is(err, cloud.ErrUnauthorized):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
	case errors.Is(err, cloud.ErrForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
	case errors.Is(err, cloud.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal server error"})
	}
}
func createDaemon(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var in struct {
			WorkspaceID string `json:"workspace_id"`
			Name        string `json:"name"`
		}
		if !parseJSON(c, &in) {
			return
		}
		out, err := s.CreateDaemon(c, accountID(c), in.WorkspaceID, in.Name)
		if err != nil {
			if errors.Is(err, cloud.ErrForbidden) || errors.Is(err, cloud.ErrNotFound) {
				serviceStatus(c, err)
			} else {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			}
			return
		}
		c.JSON(http.StatusCreated, out)
	}
}
func listDaemons(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		out, err := s.ListDaemons(c, accountID(c), c.Query("workspace_id"))
		if err != nil {
			if errors.Is(err, cloud.ErrForbidden) || errors.Is(err, cloud.ErrNotFound) {
				serviceStatus(c, err)
			} else {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			}
			return
		}
		c.JSON(http.StatusOK, out)
	}
}
func listMetrics(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := 100
		var err error
		if raw := c.Query("limit"); raw != "" {
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 100 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
				return
			}
		}
		var before *time.Time
		if raw := c.Query("before"); raw != "" {
			parsed, parseErr := time.Parse(time.RFC3339, raw)
			if parseErr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid before"})
				return
			}
			before = &parsed
		}
		out, err := s.ListMetrics(c, accountID(c), c.Param("id"), limit, before)
		if err != nil {
			serviceStatus(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}
func requestPushNotification(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var input cloud.NotificationInput
		if !parseJSON(c, &input) {
			return
		}
		out, err := s.CreatePushNotification(c, accountID(c), c.Param("id"), input)
		if err != nil {
			if errors.Is(err, cloud.ErrForbidden) || errors.Is(err, cloud.ErrNotFound) {
				serviceStatus(c, err)
			} else {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			}
			return
		}
		c.JSON(http.StatusAccepted, out)
	}
}
func getDaemon(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		out, err := s.GetDaemon(c, accountID(c), c.Param("id"))
		if err != nil {
			serviceStatus(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}
func updateDaemon(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var in struct {
			Name    *string `json:"name"`
			Enabled *bool   `json:"enabled"`
		}
		if !parseJSON(c, &in) {
			return
		}
		out, err := s.UpdateDaemon(c, accountID(c), c.Param("id"), in.Name, in.Enabled)
		if err != nil {
			if errors.Is(err, cloud.ErrForbidden) || errors.Is(err, cloud.ErrNotFound) {
				serviceStatus(c, err)
			} else {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			}
			return
		}
		c.JSON(http.StatusOK, out)
	}
}
func rotateSecret(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		secret, err := s.RotateSecret(c, accountID(c), c.Param("id"))
		if err != nil {
			serviceStatus(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"secret": secret})
	}
}
func disableDaemon(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := s.DisableDaemon(c, accountID(c), c.Param("id")); err != nil {
			serviceStatus(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}
func daemonSecret(c *gin.Context) (string, bool) {
	v := strings.TrimSpace(c.GetHeader("Authorization"))
	if !strings.HasPrefix(v, "Bearer ") {
		return "", false
	}
	secret := strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
	return secret, secret != ""
}
func ingestMetric(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		secret, ok := daemonSecret(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		var in cloud.MetricInput
		if !parseJSON(c, &in) {
			return
		}
		if err := s.IngestMetric(c, c.Param("id"), secret, in); err != nil {
			if errors.Is(err, cloud.ErrUnauthorized) {
				serviceStatus(c, err)
			} else {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			}
			return
		}
		c.Status(http.StatusNoContent)
	}
}
func listActions(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		out, err := s.ListActions(c, accountID(c), c.Param("id"))
		if err != nil {
			serviceStatus(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}
func syncActions(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		secret, ok := daemonSecret(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		var in []cloud.ActionInput
		if !parseJSON(c, &in) {
			return
		}
		if err := s.SyncActions(c, c.Param("id"), secret, in); err != nil {
			if errors.Is(err, cloud.ErrUnauthorized) {
				serviceStatus(c, err)
			} else {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			}
			return
		}
		c.Status(http.StatusNoContent)
	}
}
func createNotification(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		secret, ok := daemonSecret(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		var in cloud.NotificationInput
		if !parseJSON(c, &in) {
			return
		}
		out, err := s.CreateNotification(c, c.Param("id"), secret, in)
		if err != nil {
			if errors.Is(err, cloud.ErrUnauthorized) {
				serviceStatus(c, err)
			} else {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			}
			return
		}
		c.JSON(http.StatusCreated, gin.H{"id": out.ID, "created_at": out.CreatedAt})
	}
}
func listNotifications(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		unread, err := strconv.ParseBool(c.Query("unread"))
		if c.Query("unread") != "" && err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid unread"})
			return
		}
		limit := 50
		if raw := c.Query("limit"); raw != "" {
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 100 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
				return
			}
		}
		var before *time.Time
		if raw := c.Query("before"); raw != "" {
			parsed, e := time.Parse(time.RFC3339, raw)
			if e != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid before"})
				return
			}
			before = &parsed
		}
		out, e := s.ListNotifications(c, accountID(c), c.Query("workspace_id"), unread, c.Query("daemon_id"), limit, before)
		if e != nil {
			if errors.Is(e, cloud.ErrForbidden) || errors.Is(e, cloud.ErrNotFound) {
				serviceStatus(c, e)
			} else {
				c.JSON(http.StatusBadRequest, gin.H{"error": e.Error()})
			}
			return
		}
		c.JSON(http.StatusOK, out)
	}
}
func markRead(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := s.MarkNotificationRead(c, accountID(c), c.Param("id")); err != nil {
			serviceStatus(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}
func enqueueWebhook(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var in struct {
			Name      string `json:"name"`
			Body      string `json:"body"`
			Signature string `json:"signature"`
		}
		if !parseJSON(c, &in) {
			return
		}
		body, err := base64.StdEncoding.DecodeString(in.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		credential := credentialFrom(c)
		invokedBy := "@" + userHandle(c)
		if credential != nil {
			invokedBy = credential.Label
		}
		out, err := s.EnqueueWebhook(c, accountID(c), c.Param("id"), in.Name, body, in.Signature, invokedBy, credential)
		if err != nil {
			if errors.Is(err, cloud.ErrForbidden) || errors.Is(err, cloud.ErrNotFound) {
				serviceStatus(c, err)
				return
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"id": out.ID, "status": out.Status, "created_at": out.CreatedAt})
	}
}
func getWebhookResult(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		out, err := s.GetWebhookResult(c, accountID(c), c.Param("id"), c.Param("request_id"))
		if err != nil {
			serviceStatus(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}
func listPendingWebhooks(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		secret, ok := daemonSecret(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		limit := 50
		if raw := c.Query("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 50 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
				return
			}
			limit = parsed
		}
		out, err := s.ListPendingWebhooks(c, c.Param("id"), secret, limit)
		if err != nil {
			serviceStatus(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"requests": out})
	}
}
func completeWebhook(s *cloud.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		secret, ok := daemonSecret(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		var in struct {
			Code  int    `json:"code"`
			Body  string `json:"body"`
			Error string `json:"error"`
		}
		if !parseJSON(c, &in) {
			return
		}
		body, err := base64.StdEncoding.DecodeString(in.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		if err := s.CompleteWebhook(c, c.Param("id"), secret, c.Param("request_id"), in.Code, body, in.Error); err != nil {
			serviceStatus(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}
