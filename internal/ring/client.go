// Package ring provides gRPC access to Metoer's shared push service.
package ring

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	gen "src.solsynth.dev/sosys/go/proto"

	"src.solsynth.dev/solsynth/maidcafe/internal/cloud"
)

const maidKitAppID = "dev.solsynth.maid"

// Client publishes account notifications through Metoer/Ring.
type Client struct {
	conn   *grpc.ClientConn
	client gen.DyRingServiceClient
}

func NewClient(target string, useTLS, tlsSkipVerify bool) (*Client, error) {
	if strings.TrimSpace(target) == "" {
		return nil, fmt.Errorf("ring target is required")
	}
	var transport credentials.TransportCredentials
	if useTLS {
		transport = credentials.NewTLS(&tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: tlsSkipVerify, // #nosec G402 -- explicitly configured for internal deployments.
		})
	} else {
		transport = insecure.NewCredentials()
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(transport))
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
