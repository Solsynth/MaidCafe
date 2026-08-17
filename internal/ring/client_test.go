package ring

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	gen "src.solsynth.dev/sosys/go/proto"

	"src.solsynth.dev/solsynth/maidcafe/internal/cloud"
)

type fakeRing struct {
	gen.UnimplementedDyRingServiceServer
	requests chan *gen.DySendPushNotificationToUserRequest
}

func (f *fakeRing) SendPushNotificationToUser(
	_ context.Context,
	request *gen.DySendPushNotificationToUserRequest,
) (*emptypb.Empty, error) {
	f.requests <- request
	return &emptypb.Empty{}, nil
}

func TestPublishSendsMetoerNotification(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	fake := &fakeRing{requests: make(chan *gen.DySendPushNotificationToUserRequest, 1)}
	gen.RegisterDyRingServiceServer(grpcServer, fake)
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()
	defer listener.Close()

	client, err := NewClient(listener.Addr().String(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	event := cloud.NotificationEvent{
		EventID:        uuid.NewString(),
		AccountID:      "account-1",
		DaemonID:       "daemon-1",
		NotificationID: "notification-1",
		Kind:           "daemon.alarm.cpu_percent",
		Title:          "CPU alarm",
		Subtitle:       "From host",
		Body:           "CPU reached 90%",
		Metadata:       []byte(`{"value":90}`),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Publish(ctx, event); err != nil {
		t.Fatal(err)
	}

	select {
	case request := <-fake.requests:
		if request.UserId != event.AccountID || request.Notification == nil {
			t.Fatalf("unexpected request: %#v", request)
		}
		notification := request.Notification
		if notification.Topic != event.Kind || notification.Title != event.Title ||
			notification.Subtitle != event.Subtitle || notification.Body != event.Body ||
			string(notification.Meta) != string(event.Metadata) ||
			notification.GetAppId() != maidKitAppID || !notification.IsSavable {
			t.Fatalf("unexpected notification: %#v", notification)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
