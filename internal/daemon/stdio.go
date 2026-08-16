package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

type stdioRequest struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	Action string          `json:"action"`
	Name   string          `json:"name"`
	Body   json.RawMessage `json:"body"`
}

type stdioResponse struct {
	Type   string `json:"type"`
	ID     string `json:"id,omitempty"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Result any    `json:"result,omitempty"`
}

type stdioEvent struct {
	Type  string `json:"type"`
	Event string `json:"event"`
	Data  any    `json:"data"`
}

type stdioActionResult struct {
	request stdioRequest
	result  executionResponse
	err     *requestError
}

func (a *App) runStdio(ctx context.Context) error {
	requests := make(chan []byte)
	scanErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		bufferSize := int(a.cfg.MaxBodyBytes) + 64*1024
		scanner.Buffer(make([]byte, 64*1024), bufferSize)
		for scanner.Scan() {
			requests <- append([]byte(nil), scanner.Bytes()...)
		}
		if err := scanner.Err(); err != nil {
			scanErr <- err
			return
		}
		close(requests)
	}()

	var writeMu syncWriter
	write := func(value any) error {
		writeMu.mu.Lock()
		defer writeMu.mu.Unlock()
		return json.NewEncoder(os.Stdout).Encode(value)
	}
	if err := write(stdioEvent{Type: "event", Event: "ready", Data: map[string]any{
		"id":        a.cfg.ID,
		"transport": "stdio",
	}}); err != nil {
		return err
	}

	results := make(chan stdioActionResult)
	ticker := time.NewTicker(a.cfg.MetricsInterval)
	defer ticker.Stop()
	a.metrics.Record()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-scanErr:
			return err
		case raw, ok := <-requests:
			if !ok {
				return nil
			}
			var request stdioRequest
			if err := json.Unmarshal(raw, &request); err != nil {
				if err := write(stdioResponse{Type: "response", OK: false, Error: err.Error()}); err != nil {
					return err
				}
				continue
			}
			switch strings.ToLower(strings.TrimSpace(request.Action)) {
			case "health":
				if err := write(stdioResponse{Type: "response", ID: request.ID, OK: true, Result: map[string]any{
					"id":        a.cfg.ID,
					"mode":      "daemon",
					"transport": "stdio",
				}}); err != nil {
					return err
				}
			case "metrics":
				if err := write(stdioResponse{Type: "response", ID: request.ID, OK: true, Result: a.metrics.Collect()}); err != nil {
					return err
				}
			case "action", "invoke":
				body, err := stdioBody(request.Body)
				if err != nil {
					if err := write(stdioResponse{Type: "response", ID: request.ID, OK: false, Error: err.Error()}); err != nil {
						return err
					}
					continue
				}
				go func(request stdioRequest, body []byte) {
					result, requestErr := a.executor.RunAction(ctx, request.Name, body, "stdio", "stdio")
					results <- stdioActionResult{request: request, result: result, err: requestErr}
				}(request, body)
			case "shutdown":
				if err := write(stdioResponse{Type: "response", ID: request.ID, OK: true}); err != nil {
					return err
				}
				return nil
			default:
				if err := write(stdioResponse{Type: "response", ID: request.ID, OK: false, Error: "unsupported action"}); err != nil {
					return err
				}
			}
		case result := <-results:
			if result.err != nil {
				if err := write(stdioResponse{Type: "response", ID: result.request.ID, OK: false, Error: result.err.message}); err != nil {
					return err
				}
				continue
			}
			if err := write(stdioResponse{Type: "response", ID: result.request.ID, OK: result.result.OK, Result: result.result}); err != nil {
				return err
			}
		case <-ticker.C:
			metrics := a.metrics.Record()
			if a.publisher != nil {
				a.publisher.PublishMetrics(context.Background(), metrics)
				a.publisher.PublishActions(context.Background(), a.cfg.Actions)
			}
			if err := write(stdioEvent{Type: "event", Event: "metrics", Data: metrics}); err != nil {
				return err
			}
		}
	}
}

func stdioBody(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []byte(text), nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("body must be valid JSON or a string")
	}
	return raw, nil
}

type syncWriter struct {
	mu sync.Mutex
}
