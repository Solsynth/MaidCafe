package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// subscriberBufferSize is the per-subscriber frame queue. Broadcasts drop the
// oldest buffered frame rather than block the collector when a slow client
// falls behind (coalesce semantics).
const subscriberBufferSize = 8

// sseHeartbeatInterval is how often a comment frame is written while no event
// frame was sent, keeping intermediaries from closing the idle stream.
const sseHeartbeatInterval = 15 * time.Second

// streamEventTypes is the whitelist of subscribable event names for
// GET /api/v1/stream. hello is not subscribable: it is always sent first.
var streamEventTypes = []string{"metric", "containers", "images", "processes", "systemd"}

// Subscriber is one SSE client's handle on a StreamHub. C delivers complete
// SSE frames (event/data/blank line) for the requested types.
type Subscriber struct {
	hub   *StreamHub
	Types []string
	C     <-chan []byte
	ch    chan []byte
}

// StreamHub fans pre-marshaled event payloads out to per-type subscribers.
// Broadcast never blocks: a subscriber with a full buffer drops its oldest
// queued frame. Collection is gated on Subscribers(type) > 0, so an idle hub
// costs nothing.
type StreamHub struct {
	mu   sync.RWMutex
	all  map[*Subscriber]struct{}
	subs map[string]map[*Subscriber]struct{}
}

func NewStreamHub() *StreamHub {
	return &StreamHub{
		all:  make(map[*Subscriber]struct{}),
		subs: make(map[string]map[*Subscriber]struct{}),
	}
}

// Subscribe registers a subscriber for the given event types. An empty slice
// subscribes to nothing; callers that want "all events" pass the full list.
// The returned handle owns a buffered channel closed by Unsubscribe.
func (h *StreamHub) Subscribe(types []string) *Subscriber {
	unique := make([]string, 0, len(types))
	seen := make(map[string]struct{}, len(types))
	for _, raw := range types {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		unique = append(unique, t)
	}
	s := &Subscriber{
		hub:   h,
		Types: unique,
		ch:    make(chan []byte, subscriberBufferSize),
	}
	s.C = s.ch
	h.mu.Lock()
	defer h.mu.Unlock()
	h.all[s] = struct{}{}
	for _, t := range unique {
		bucket := h.subs[t]
		if bucket == nil {
			bucket = make(map[*Subscriber]struct{})
			h.subs[t] = bucket
		}
		bucket[s] = struct{}{}
	}
	return s
}

// Unsubscribe removes the subscriber from every type bucket and closes its
// channel. Safe to call multiple times; Broadcast holds the same lock, so no
// send ever races the close.
func (h *StreamHub) Unsubscribe(s *Subscriber) {
	if s == nil || s.hub != h {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.all[s]; !ok {
		return
	}
	delete(h.all, s)
	for _, t := range s.Types {
		if bucket := h.subs[t]; bucket != nil {
			delete(bucket, s)
			if len(bucket) == 0 {
				delete(h.subs, t)
			}
		}
	}
	close(s.ch)
}

// Broadcast sends one pre-marshaled event payload to every subscriber of the
// type, wrapped in a standard SSE frame. The frame bytes are shared across all
// subscribers (marshal once, fan out). A full subscriber buffer drops its
// oldest frame; the broadcaster never blocks.
func (h *StreamHub) Broadcast(event string, data []byte) {
	frame := []byte("event: " + event + "\ndata: " + string(data) + "\n\n")
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs[event] {
		select {
		case s.ch <- frame:
		default:
			// Slow client: drop the oldest queued frame, then push the new one.
			select {
			case <-s.ch:
			default:
			}
			select {
			case s.ch <- frame:
			default:
			}
		}
	}
}

// Subscribers reports how many subscribers currently want the given type.
func (h *StreamHub) Subscribers(event string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs[event])
}

// Len reports the total number of subscribers across all types.
func (h *StreamHub) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.all)
}

// parseEventsParam validates the comma-separated events whitelist. An empty
// value returns nil, meaning "all event types" to the caller.
func parseEventsParam(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	known := make(map[string]struct{}, len(streamEventTypes))
	for _, t := range streamEventTypes {
		known[t] = struct{}{}
	}
	var types []string
	seen := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		t := strings.TrimSpace(part)
		if t == "" {
			continue
		}
		if _, ok := known[t]; !ok {
			return nil, fmt.Errorf("unknown event type %q", t)
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		types = append(types, t)
	}
	return types, nil
}

// handleStream serves GET /api/v1/stream. It clears the server-level write
// deadline (the 10s WriteTimeout would otherwise kill the stream), sends the
// hello frame first, then fans subscribed frames out with a 15s heartbeat.
func handleStream(c *gin.Context, hub *StreamHub, cfg config.DaemonConfig) {
	types, err := parseEventsParam(c.Query("events"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error()})
		return
	}
	if len(types) == 0 {
		types = append([]string(nil), streamEventTypes...)
	}
	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	// The server's WriteTimeout would kill a long-lived stream; clear it.
	// gin's ResponseWriter unwraps to the net/http writer, which supports it.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		// Non-fatal: some wrappers (e.g. test recorders) do not support it.
		_ = err
	}
	w.WriteHeader(http.StatusOK)
	w.Flush()

	sub := hub.Subscribe(types)
	defer hub.Unsubscribe(sub)

	hello := gin.H{
		"stream":  "v1",
		"version": cfg.Version,
		"intervals": gin.H{
			"metric":     int(cfg.StreamInterval.Seconds()),
			"containers": int(cfg.ContainersInterval.Seconds()),
			"images":     int(cfg.ImagesInterval.Seconds()),
			"processes":  int(cfg.ProcessesInterval.Seconds()),
			"systemd":    int(cfg.SystemdInterval.Seconds()),
		},
	}
	helloData, err := json.Marshal(hello)
	if err != nil || !writeSSEFrame(w, "hello", helloData) {
		return
	}
	lastWrite := time.Now()

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case frame, ok := <-sub.C:
			if !ok {
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			lastWrite = time.Now()
			w.Flush()
		case <-heartbeat.C:
			if time.Since(lastWrite) < sseHeartbeatInterval {
				continue
			}
			if _, err := w.WriteString(": ping\n\n"); err != nil {
				return
			}
			lastWrite = time.Now()
			w.Flush()
		}
	}
}

func writeSSEFrame(w gin.ResponseWriter, event string, data []byte) bool {
	if _, err := w.WriteString("event: " + event + "\n"); err != nil {
		return false
	}
	if _, err := w.WriteString("data: "); err != nil {
		return false
	}
	if _, err := w.Write(data); err != nil {
		return false
	}
	if _, err := w.WriteString("\n\n"); err != nil {
		return false
	}
	w.Flush()
	return true
}
