package eventbus

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"src.solsynth.dev/solsynth/maidcafe/internal/cloud"
	shared "src.solsynth.dev/sosys/go/pkg/eventbus"
)

// Bus is nil-safe and only used for cloud push fan-out.
type Bus struct { conn *nats.Conn; bus *shared.Bus }

func New(url, appName string) (*Bus, error) {
	if url == "" { return nil, nil }
	conn, err := nats.Connect(url, nats.Name(appName), nats.MaxReconnects(-1), nats.ReconnectWait(2*time.Second))
	if err != nil { return nil, fmt.Errorf("connect eventbus: %w", err) }
	b, err := shared.New(conn)
	if err != nil { conn.Close(); return nil, fmt.Errorf("initialize eventbus: %w", err) }
	return &Bus{conn:conn,bus:b}, nil
}
func (b *Bus) Close() { if b != nil && b.conn != nil { b.conn.Close() } }
func (b *Bus) Publish(ctx context.Context, evt cloud.NotificationEvent) error {
	if b == nil || b.bus == nil { return nil }
	return b.bus.PublishJetStream(ctx, "maidcafe.notification.v1", "maidcafe_events", evt)
}
