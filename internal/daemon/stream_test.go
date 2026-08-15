package daemon

import (
	"fmt"
	"testing"
	"time"
)

func frameFor(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

func TestStreamHubFanOutToMultipleSubscribers(t *testing.T) {
	hub := NewStreamHub()
	const n = 5
	subs := make([]*Subscriber, n)
	for i := range subs {
		subs[i] = hub.Subscribe([]string{"metric"})
		defer hub.Unsubscribe(subs[i])
	}
	hub.Broadcast("metric", []byte(`{"v":1}`))
	for i, sub := range subs {
		select {
		case frame := <-sub.C:
			if string(frame) != frameFor("metric", `{"v":1}`) {
				t.Fatalf("subscriber %d frame = %q", i, frame)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d did not receive the broadcast", i)
		}
	}
}

func TestStreamHubDropOnOverflowNeverBlocks(t *testing.T) {
	hub := NewStreamHub()
	sub := hub.Subscribe([]string{"metric"})
	defer hub.Unsubscribe(sub)
	for i := 1; i <= subscriberBufferSize; i++ {
		hub.Broadcast("metric", []byte(fmt.Sprintf("frame-%d", i)))
	}
	// The buffer is now full. The next broadcast must drop the oldest frame
	// and return without blocking on the slow (non-reading) subscriber.
	done := make(chan struct{})
	go func() {
		hub.Broadcast("metric", []byte("frame-9"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("broadcast blocked on a full subscriber buffer")
	}
	// Buffer now holds frames 2..9: frame-1 was dropped as the oldest.
	for want := 2; want <= 9; want++ {
		select {
		case frame := <-sub.C:
			if string(frame) != frameFor("metric", fmt.Sprintf("frame-%d", want)) {
				t.Fatalf("frame = %q, want frame-%d", frame, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing frame-%d after overflow", want)
		}
	}
}

func TestStreamHubUnsubscribeStopsDelivery(t *testing.T) {
	hub := NewStreamHub()
	sub := hub.Subscribe([]string{"metric"})
	hub.Broadcast("metric", []byte("one"))
	select {
	case <-sub.C:
	case <-time.After(time.Second):
		t.Fatal("initial broadcast was not delivered")
	}
	hub.Unsubscribe(sub)
	if hub.Len() != 0 || hub.Subscribers("metric") != 0 {
		t.Fatalf("unsubscribe left state: Len()=%d Subscribers(metric)=%d", hub.Len(), hub.Subscribers("metric"))
	}
	hub.Broadcast("metric", []byte("two"))
	select {
	case frame, ok := <-sub.C:
		if ok {
			t.Fatalf("delivered %q after unsubscribe", frame)
		}
	case <-time.After(100 * time.Millisecond):
	}
	// Unsubscribing twice must be safe.
	hub.Unsubscribe(sub)
}

func TestStreamHubPerTypeSubscriberCounts(t *testing.T) {
	hub := NewStreamHub()
	a := hub.Subscribe([]string{"metric"})
	b := hub.Subscribe([]string{"metric", "containers"})
	c := hub.Subscribe([]string{"processes"})
	defer hub.Unsubscribe(a)
	defer hub.Unsubscribe(b)
	defer hub.Unsubscribe(c)
	if got := hub.Subscribers("metric"); got != 2 {
		t.Fatalf("Subscribers(metric) = %d, want 2", got)
	}
	if got := hub.Subscribers("containers"); got != 1 {
		t.Fatalf("Subscribers(containers) = %d, want 1", got)
	}
	if got := hub.Subscribers("processes"); got != 1 {
		t.Fatalf("Subscribers(processes) = %d, want 1", got)
	}
	if got := hub.Subscribers("systemd"); got != 0 {
		t.Fatalf("Subscribers(systemd) = %d, want 0", got)
	}
	if got := hub.Len(); got != 3 {
		t.Fatalf("Len() = %d, want 3", got)
	}
	// A subscriber only receives events for its requested types.
	hub.Broadcast("containers", []byte("c1"))
	select {
	case frame := <-b.C:
		if string(frame) != frameFor("containers", "c1") {
			t.Fatalf("containers frame = %q", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("containers subscriber missed its broadcast")
	}
	select {
	case frame := <-a.C:
		t.Fatalf("metric-only subscriber received containers frame %q", frame)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestParseEventsParam(t *testing.T) {
	if types, err := parseEventsParam(""); err != nil || types != nil {
		t.Fatalf("empty events = %#v, %v; want nil, nil", types, err)
	}
	if types, err := parseEventsParam("metric,containers,images, processes "); err != nil {
		t.Fatalf("valid events rejected: %v", err)
	} else if len(types) != 4 || types[0] != "metric" || types[1] != "containers" || types[2] != "images" || types[3] != "processes" {
		t.Fatalf("valid events parsed as %#v", types)
	}
	if _, err := parseEventsParam("metric,bogus"); err == nil {
		t.Fatal("unknown event type accepted")
	}
	if types, err := parseEventsParam("metric,metric"); err != nil || len(types) != 1 {
		t.Fatalf("duplicate events = %#v, %v; want [metric], nil", types, err)
	}
}
