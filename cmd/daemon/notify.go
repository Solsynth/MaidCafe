package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// notifyCmd sends a custom notification through a locally running daemon.
// It reads the daemon config for the metrics secret and listen address, then
// POSTs to the daemon's /api/v1/notifications endpoint so the payload is
// relayed to the cloud and on to the user's Metoer feed.
func notifyCmd(args []string) {
	fs := flag.NewFlagSet("notify", flag.ExitOnError)
	configPath := fs.String("config", "/etc/maidcafe/config.toml", "daemon configuration file path")
	title := fs.String("title", "", "notification title (required)")
	subtitle := fs.String("subtitle", "", "notification subtitle")
	body := fs.String("body", "", "notification body (required)")
	kind := fs.String("kind", "", "notification kind (default: daemon.notification)")
	metadata := fs.String("metadata", "", "extra metadata as a JSON object")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: maidcafe-daemon notify [flags]\n\n")
		fmt.Fprintf(fs.Output(), "Send a custom notification through the local MaidCafe daemon to your\nMetoer feed. Requires the daemon config (for the metrics secret and\nlisten address) and a running daemon with http transport.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fail("load config: %v", err)
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Daemon.Transport), "stdio") {
		fail("the notify command needs the daemon's http transport; transport is %q", cfg.Daemon.Transport)
	}
	if strings.TrimSpace(cfg.Daemon.MetricsSecret) == "" {
		fail("daemon metricsSecret is empty; cannot authenticate against the local daemon")
	}
	titleText := strings.TrimSpace(*title)
	bodyText := strings.TrimSpace(*body)
	if titleText == "" || bodyText == "" {
		fail("--title and --body are required")
	}
	meta := map[string]any{}
	if strings.TrimSpace(*metadata) != "" {
		if err := json.Unmarshal([]byte(*metadata), &meta); err != nil {
			fail("--metadata must be a JSON object: %v", err)
		}
	}
	kindText := strings.TrimSpace(*kind)
	if kindText == "" {
		kindText = "daemon.notification"
	}
	payload, err := json.Marshal(map[string]any{
		"kind":     kindText,
		"title":    titleText,
		"subtitle": strings.TrimSpace(*subtitle),
		"body":     bodyText,
		"metadata": meta,
	})
	if err != nil {
		fail("encode payload: %v", err)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(
		http.MethodPost,
		"http://"+cfg.Daemon.Listen+"/api/v1/notifications",
		bytes.NewReader(payload),
	)
	if err != nil {
		fail("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Daemon.MetricsSecret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		fail("send notification: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fail("daemon returned %s", resp.Status)
	}
	fmt.Println("notification sent")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "maidcafe-daemon notify: "+format+"\n", args...)
	os.Exit(1)
}
