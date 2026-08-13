package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"src.solsynth.dev/solsynth/maidcafe/internal/cloud"
	"src.solsynth.dev/solsynth/maidcafe/internal/database"
)

type routePublisher struct{}
func (routePublisher) Publish(context.Context, cloud.NotificationEvent) error { return nil }
func TestCloudHealthAndCredentialBoundary(t *testing.T) { db,err:=database.NewSQLite();if err!=nil{t.Fatal(err)};defer db.Close();if err:=db.AutoMigrate();err!=nil{t.Fatal(err)};svc:=cloud.NewService(db,routePublisher{});router:=NewRouter(nil,svc,nil);health:=httptest.NewRecorder();router.ServeHTTP(health,httptest.NewRequest(http.MethodGet,"/health",nil));if health.Code!=http.StatusOK{t.Fatalf("health %d",health.Code)};var healthJSON map[string]any;if err:=json.Unmarshal(health.Body.Bytes(),&healthJSON);err!=nil||healthJSON["mode"]!="cloud"{t.Fatalf("health body %s",health.Body)};unauth:=httptest.NewRecorder();router.ServeHTTP(unauth,httptest.NewRequest(http.MethodGet,"/api/daemons",nil));if unauth.Code!=http.StatusUnauthorized{t.Fatalf("unauthenticated user route %d",unauth.Code)};daemon,err:=svc.CreateDaemon(context.Background(),"account-a","host");if err!=nil{t.Fatal(err)};metric:=httptest.NewRequest(http.MethodPost,"/api/daemons/"+daemon.ID+"/metrics",strings.NewReader(`{"sent_at":"2026-08-12T00:00:00Z"}`));metric.Header.Set("Authorization","Bearer "+daemon.Secret);got:=httptest.NewRecorder();router.ServeHTTP(got,metric);if got.Code!=http.StatusNoContent{t.Fatalf("daemon metric route %d %s",got.Code,got.Body)} }
