// Package ring provides gRPC access to Metoer's shared push service.
package ring

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"src.solsynth.dev/sosys/lighthouse/shared/platform/grpctls"
	gen "src.solsynth.dev/sosys/lighthouse/shared/proto"

	"src.solsynth.dev/solsynth/maidcafe/internal/cloud"
	"src.solsynth.dev/solsynth/maidcafe/internal/grpctl"
)

const maidKitAppID = "dev.solsynth.maid"

// Client publishes account notifications through Metoer/Ring.
type Client struct {
	conn   *grpc.ClientConn
	client gen.DyRingServiceClient
}

// NewClient dials Metoer's push service. files carries the fleet CA bundle and
// the certificate this cloud presents to peers enforcing mutual TLS.
func NewClient(target string, useTLS, tlsSkipVerify bool, files grpctls.Files) (*Client, error) {
	if strings.TrimSpace(target) == "" {
		return nil, fmt.Errorf("ring target is required")
	}
	conn, err := grpctl.Dial(target, useTLS, tlsSkipVerify, files)
	if err != nil {
		return nil, fmt.Errorf("connect to ring: %w", err)
	}
	return &Client{conn: conn, client: gen.NewDyRingServiceClient(conn)}, nil
}

func (c *Client) Publish(ctx context.Context, event cloud.NotificationEvent) error {
	if c == nil || c.client == nil {
		return nil
	}
	appID := maidKitAppID
	_, err := c.client.SendPushNotificationToUser(ctx, &gen.DySendPushNotificationToUserRequest{
		UserId: event.AccountID,
		Notification: &gen.DyPushNotification{
			Topic:     event.Kind,
			Title:     event.Title,
			Subtitle:  event.Subtitle,
			Body:      event.Body,
			Meta:      event.Metadata,
			IsSavable: true,
			AppId:     &appID,
		},
	})
	return err
}

func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}
