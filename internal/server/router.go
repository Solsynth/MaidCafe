package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"src.solsynth.dev/solsynth/maidcafe/internal/cloud"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
	"src.solsynth.dev/solsynth/maidcafe/internal/handler"
	dyauth "src.solsynth.dev/sosys/go/pkg/auth"
)

func NewRouter(_ *config.Config, svc *cloud.Service, authenticator dyauth.TokenAuthenticator) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	userAuth := func(c *gin.Context) {
		token, ok := dyauth.ExtractToken(c.Request)
		if !ok || authenticator == nil { c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error":"unauthorized"}); return }
		result, err := dyauth.AuthenticateRequest(c.Request.Context(), authenticator, c.Request)
		if err != nil { c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error":"unauthorized"}); return }
		dyauth.WithAuth(c, result, token)
		c.Next()
	}
	handler.RegisterRoutes(r, svc, userAuth)
	return r
}
