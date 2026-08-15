package eventbus

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"src.solsynth.dev/solsynth/maidcafe/internal/cloud"
	shared "src.solsynth.dev/sosys/go/pkg/eventbus"
)

// Bus is nil-safe and only used for cloud push fan-out.
type Bus struct {
	conn   *nats.Conn
	bus    *shared.Bus
	prefix string
}

// New connects to NATS when url is non-empty. subjectPrefix, when non-empty,
// is prepended to the published subject (e.g. "staging" ->
// "staging.maidcafe.notification.v1"), letting multiple deployments share one
// NATS server without subject collisions. Leading/trailing dots and spaces
// are stripped; an empty or dot-only prefix keeps the plain subject.
func New(url, appName, subjectPrefix string) (*Bus, error) {
	if url == "" {
		return nil, nil
	}
	prefix := strings.Trim(strings.TrimSpace(subjectPrefix), ".")
	conn, err := nats.Connect(url, nats.Name(appName), nats.MaxReconnects(-1), nats.ReconnectWait(2*time.Second))
	if err != nil {
		return nil, fmt.Errorf("connect eventbus: %w", err)
	}
	b, err := shared.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("initialize eventbus: %w", err)
	}
	return &Bus{conn: conn, bus: b, prefix: prefix}, nil
}
func (b *Bus) Close() {
	if b != nil && b.conn != nil {
		b.conn.Close()
	}
}
func (b *Bus) Publish(ctx context.Context, evt cloud.NotificationEvent) error {
	if b == nil || b.bus == nil {
		return nil
	}
	subject := "maidcafe.notification.v1"
	if b.prefix != "" {
		subject = b.prefix + "." + subject
	}
	return b.bus.PublishJetStream(ctx, subject, "maidcafe_events", evt)
}
