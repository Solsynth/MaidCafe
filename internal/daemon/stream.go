package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
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
var streamEventTypes = []string{"metric", "containers", "images", "processes", "systemd", "runtimes", "databaseMetrics", "logs"}

// Subscriber is one SSE client's handle on a StreamHub. C delivers complete
// SSE frames (event/data/blank line) for the requested types.
type Subscriber struct {
	hub   *StreamHub
	Types []string
	// ProcessesLimit caps how many processes this subscriber receives on
	// `processes` frames. 0 means the complete table (no cap).
	ProcessesLimit int
	C              <-chan []byte
	ch             chan []byte
}

// SubscribeOption tweaks a new subscriber.
type SubscribeOption func(*Subscriber)

// WithProcessesLimit caps the subscriber's `processes` frames at limit rows.
// 0 (the default) delivers the complete process table.
func WithProcessesLimit(limit int) SubscribeOption {
	return func(s *Subscriber) { s.ProcessesLimit = limit }
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
func (h *StreamHub) Subscribe(types []string, opts ...SubscribeOption) *Subscriber {
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
	for _, opt := range opts {
		opt(s)
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
	h.mu.Lock()
	defer h.mu.Unlock()
	h.broadcastLocked(event, data)
}

// BroadcastProcesses fans a process snapshot out to `processes` subscribers,
// trimming each to its own limit (0 = all). The full table is collected once;
// a capped subscriber only pays for the rows it receives. Frames are shared
// when every subscriber wants the same slice, and re-marshaled per subscriber
// otherwise, so per-subscriber limits never block the broadcaster.
func (h *StreamHub) BroadcastProcesses(entries []processEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	bucket := h.subs["processes"]
	if len(bucket) == 0 {
		return
	}
	// Common cap? Slice once and share one frame (0 = all).
	common := -1
	shared := true
	for s := range bucket {
		if common == -1 {
			common = s.ProcessesLimit
			continue
		}
		if s.ProcessesLimit != common {
			shared = false
			break
		}
	}
	if shared {
		limited := entries
		if common > 0 && len(entries) > common {
			limited = entries[:common]
		}
		data, err := json.Marshal(processesPayload{Processes: limited})
		if err != nil {
			return
		}
		h.broadcastLocked("processes", data)
		return
	}
	for s := range bucket {
		limited := entries
		if s.ProcessesLimit > 0 && len(entries) > s.ProcessesLimit {
			limited = entries[:s.ProcessesLimit]
		}
		data, err := json.Marshal(processesPayload{Processes: limited})
		if err != nil {
			continue
		}
		h.pushLocked(s, []byte("event: processes\ndata: "+string(data)+"\n\n"))
	}
}

func (h *StreamHub) broadcastLocked(event string, data []byte) {
	frame := []byte("event: " + event + "\ndata: " + string(data) + "\n\n")
	for s := range h.subs[event] {
		h.pushLocked(s, frame)
	}
}

// pushLocked queues a frame on a subscriber, dropping the oldest queued frame
// when the buffer is full so a slow client never blocks the broadcaster.
func (h *StreamHub) pushLocked(s *Subscriber, frame []byte) {
	select {
	case s.ch <- frame:
	default:
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

// maxProcessesLimit is the largest per-request process cap accepted on
// GET /api/v1/processes and the stream's processesLimit override.
const maxProcessesLimit = 10000

// parseProcessesLimit parses a `limit`/`processesLimit` query value. An empty
// value falls back to defaultLimit; 0 requests the complete process table;
// anything else must be within 1..maxProcessesLimit.
func parseProcessesLimit(raw string, defaultLimit int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return defaultLimit, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("limit must be an integer")
	}
	if n < 0 || n > maxProcessesLimit {
		return 0, fmt.Errorf("limit must be 0 (all) or between 1 and %d", maxProcessesLimit)
	}
	return n, nil
}

// handleStream serves GET /api/v1/stream. It clears the server-level write
// deadline (the 10s WriteTimeout would otherwise kill the stream), sends the
// hello frame first, then fans subscribed frames out with a 15s heartbeat.
// Intervals come from the reloadable state so the hello reflects the current
// configuration after a reload.
func handleStream(c *gin.Context, hub *StreamHub, rt *reloadableConfig) {
	types, err := parseEventsParam(c.Query("events"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error()})
		return
	}
	if len(types) == 0 {
		types = append([]string(nil), streamEventTypes...)
	}
	processesLimit, err := parseProcessesLimit(c.Query("processesLimit"), rt.processesLimit)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error()})
		return
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

	sub := hub.Subscribe(types, WithProcessesLimit(processesLimit))
	defer hub.Unsubscribe(sub)

	hello := gin.H{
		"stream":  "v1",
		"version": rt.version,
		"intervals": gin.H{
			"metric":          int(rt.intervals.stream.Seconds()),
			"containers":      int(rt.intervals.containers.Seconds()),
			"images":          int(rt.intervals.images.Seconds()),
			"processes":       int(rt.intervals.processes.Seconds()),
			"systemd":         int(rt.intervals.systemd.Seconds()),
			"runtimes":        int(rt.intervals.runtimes.Seconds()),
			"databaseMetrics": int(rt.intervals.database.Seconds()),
			"logs":            int(rt.intervals.logs.Seconds()),
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
