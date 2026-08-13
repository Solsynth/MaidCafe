package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"src.solsynth.dev/solsynth/maidcafe/internal/cloud"
	dyauth "src.solsynth.dev/sosys/go/pkg/auth"
)

func RegisterRoutes(r *gin.Engine, svc *cloud.Service, userAuth gin.HandlerFunc) {
	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok":true,"mode":"cloud"}) })
	user := r.Group("/api")
	user.Use(userAuth, requireUser())
	user.GET("/daemons", listDaemons(svc))
	user.GET("/daemons/:id", getDaemon(svc))
	user.PATCH("/daemons/:id", updateDaemon(svc))
	user.POST("/daemons/:id/rotate-secret", rotateSecret(svc))
	user.DELETE("/daemons/:id", disableDaemon(svc))
	user.GET("/notifications", listNotifications(svc))
	user.POST("/notifications/:id/read", markRead(svc))
	daemon := r.Group("/api/daemons/:id")
	daemon.POST("/metrics", ingestMetric(svc))
	daemon.POST("/notifications", createNotification(svc))
}

func requireUser() gin.HandlerFunc { return func(c *gin.Context) { if _,_,ok:=dyauth.GetAuth(c); !ok { c.AbortWithStatusJSON(http.StatusUnauthorized,gin.H{"error":"unauthorized"});return };c.Next() } }
func accountID(c *gin.Context) string { result,_,_:=dyauth.GetAuth(c); return result.Account.GetId() }
func parseJSON(c *gin.Context, dst any) bool { c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20); decoder:=json.NewDecoder(c.Request.Body);decoder.DisallowUnknownFields();if err:=decoder.Decode(dst);err!=nil{c.AbortWithStatusJSON(http.StatusBadRequest,gin.H{"error":"invalid JSON"});return false};var extra any;if err:=decoder.Decode(&extra);err!=io.EOF{c.AbortWithStatusJSON(http.StatusBadRequest,gin.H{"error":"invalid JSON"});return false};return true }
func serviceStatus(c *gin.Context,err error){switch{case errors.Is(err,cloud.ErrUnauthorized):c.JSON(http.StatusUnauthorized,gin.H{"error":"unauthorized"});case errors.Is(err,cloud.ErrForbidden):c.JSON(http.StatusForbidden,gin.H{"error":"forbidden"});case errors.Is(err,cloud.ErrNotFound):c.JSON(http.StatusNotFound,gin.H{"error":"not found"});default:c.JSON(http.StatusInternalServerError,gin.H{"error":"internal server error"})}}
func createDaemon(s *cloud.Service)gin.HandlerFunc{return func(c *gin.Context){var in struct{Name string `json:"name"`};if !parseJSON(c,&in){return};out,err:=s.CreateDaemon(c,accountID(c),in.Name);if err!=nil{c.JSON(http.StatusBadRequest,gin.H{"error":err.Error()});return};c.JSON(http.StatusCreated,out)}}
func listDaemons(s *cloud.Service)gin.HandlerFunc{return func(c *gin.Context){out,err:=s.ListDaemons(c,accountID(c));if err!=nil{serviceStatus(c,err);return};c.JSON(http.StatusOK,out)}}
func getDaemon(s *cloud.Service)gin.HandlerFunc{return func(c *gin.Context){out,err:=s.GetDaemon(c,accountID(c),c.Param("id"));if err!=nil{serviceStatus(c,err);return};c.JSON(http.StatusOK,out)}}
func updateDaemon(s *cloud.Service)gin.HandlerFunc{return func(c *gin.Context){var in struct{Name *string `json:"name"`;Enabled *bool `json:"enabled"`};if !parseJSON(c,&in){return};out,err:=s.UpdateDaemon(c,accountID(c),c.Param("id"),in.Name,in.Enabled);if err!=nil{if errors.Is(err,cloud.ErrForbidden)||errors.Is(err,cloud.ErrNotFound){serviceStatus(c,err)}else{c.JSON(http.StatusBadRequest,gin.H{"error":err.Error()})};return};c.JSON(http.StatusOK,out)}}
func rotateSecret(s *cloud.Service)gin.HandlerFunc{return func(c *gin.Context){secret,err:=s.RotateSecret(c,accountID(c),c.Param("id"));if err!=nil{serviceStatus(c,err);return};c.JSON(http.StatusOK,gin.H{"secret":secret})}}
func disableDaemon(s *cloud.Service)gin.HandlerFunc{return func(c *gin.Context){if err:=s.DisableDaemon(c,accountID(c),c.Param("id"));err!=nil{serviceStatus(c,err);return};c.Status(http.StatusNoContent)}}
func daemonSecret(c *gin.Context)(string,bool){v:=strings.TrimSpace(c.GetHeader("Authorization"));if !strings.HasPrefix(v,"Bearer "){return "",false};secret:=strings.TrimSpace(strings.TrimPrefix(v,"Bearer "));return secret,secret!=""}
func ingestMetric(s *cloud.Service)gin.HandlerFunc{return func(c *gin.Context){secret,ok:=daemonSecret(c);if !ok{c.JSON(http.StatusUnauthorized,gin.H{"error":"unauthorized"});return};var in cloud.MetricInput;if !parseJSON(c,&in){return};if err:=s.IngestMetric(c,c.Param("id"),secret,in);err!=nil{if errors.Is(err,cloud.ErrUnauthorized){serviceStatus(c,err)}else{c.JSON(http.StatusBadRequest,gin.H{"error":err.Error()})};return};c.Status(http.StatusNoContent)}}
func createNotification(s *cloud.Service)gin.HandlerFunc{return func(c *gin.Context){secret,ok:=daemonSecret(c);if !ok{c.JSON(http.StatusUnauthorized,gin.H{"error":"unauthorized"});return};var in cloud.NotificationInput;if !parseJSON(c,&in){return};out,err:=s.CreateNotification(c,c.Param("id"),secret,in);if err!=nil{if errors.Is(err,cloud.ErrUnauthorized){serviceStatus(c,err)}else{c.JSON(http.StatusBadRequest,gin.H{"error":err.Error()})};return};c.JSON(http.StatusCreated,gin.H{"id":out.ID,"created_at":out.CreatedAt})}}
func listNotifications(s *cloud.Service)gin.HandlerFunc{return func(c *gin.Context){unread,err:=strconv.ParseBool(c.Query("unread"));if c.Query("unread")!=""&&err!=nil{c.JSON(http.StatusBadRequest,gin.H{"error":"invalid unread"});return};limit:=50;if raw:=c.Query("limit");raw!=""{limit,err=strconv.Atoi(raw);if err!=nil||limit<1||limit>100{c.JSON(http.StatusBadRequest,gin.H{"error":"invalid limit"});return}};var before *time.Time;if raw:=c.Query("before");raw!=""{parsed,e:=time.Parse(time.RFC3339,raw);if e!=nil{c.JSON(http.StatusBadRequest,gin.H{"error":"invalid before"});return};before=&parsed};out,e:=s.ListNotifications(c,accountID(c),unread,c.Query("daemon_id"),limit,before);if e!=nil{serviceStatus(c,e);return};c.JSON(http.StatusOK,out)}}
func markRead(s *cloud.Service)gin.HandlerFunc{return func(c *gin.Context){if err:=s.MarkNotificationRead(c,accountID(c),c.Param("id"));err!=nil{serviceStatus(c,err);return};c.Status(http.StatusNoContent)}}
