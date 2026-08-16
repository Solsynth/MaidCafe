package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"src.solsynth.dev/solsynth/maidcafe/internal/cloud"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
	"src.solsynth.dev/solsynth/maidcafe/internal/handler"
	dyauth "src.solsynth.dev/sosys/go/pkg/auth"
	gen "src.solsynth.dev/sosys/go/proto"
)

func NewRouter(_ *config.Config, svc *cloud.Service, authenticator dyauth.TokenAuthenticator) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	userAuth := func(c *gin.Context) {
		token, ok := dyauth.ExtractToken(c.Request)
		if !ok || authenticator == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		// User-level API credentials (CI/CD) bypass the Solarpass exchange:
		// the token is looked up by hash and the credential's account is
		// synthesized so the handler stack works unchanged; scope
		// enforcement happens in the service layer.
		if strings.HasPrefix(token.Token, cloud.CredentialTokenPrefix) {
			credential, err := svc.CredentialByToken(c, token.Token)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
				return
			}
			dyauth.WithAuth(c, &dyauth.AuthResult{
				Account: &gen.DyAccount{Id: credential.AccountID},
			}, token)
			handler.WithCredential(c, credential)
			c.Next()
			return
		}
		result, err := dyauth.AuthenticateRequest(c.Request.Context(), authenticator, c.Request)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		dyauth.WithAuth(c, result, token)
		c.Next()
	}
	handler.RegisterRoutes(r, svc, userAuth)
	return r
}
