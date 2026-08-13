package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	App      AppConfig      `mapstructure:"app"`
	HTTP     HTTPConfig     `mapstructure:"http"`
	Database DatabaseConfig `mapstructure:"database"`
	Auth     AuthConfig     `mapstructure:"auth"`
	Eventbus EventbusConfig `mapstructure:"eventbus"`
	Ring     RingConfig     `mapstructure:"ring"`
	Daemon   DaemonConfig   `mapstructure:"daemon"`
}

type AppConfig struct {
	Name string `mapstructure:"name"`
}
type HTTPConfig struct {
	Port string `mapstructure:"port"`
}
type DatabaseConfig struct {
	DSN string `mapstructure:"dsn"`
}
type AuthConfig struct {
	Target        string `mapstructure:"target"`
	UseTLS        bool   `mapstructure:"useTLS"`
	TLSSkipVerify bool   `mapstructure:"tlsSkipVerify"`
}
type RingConfig struct {
	Target        string `mapstructure:"target"`
	UseTLS        bool   `mapstructure:"useTLS"`
	TLSSkipVerify bool   `mapstructure:"tlsSkipVerify"`
}
type EventbusConfig struct {
	URL string `mapstructure:"url"`
}
type DaemonConfig struct {
	ID                string          `mapstructure:"id"`
	Listen            string          `mapstructure:"listen"`
	CloudURL          string          `mapstructure:"cloudUrl"`
	CloudSecret       string          `mapstructure:"cloudSecret"`
	MetricsInterval   time.Duration   `mapstructure:"metricsInterval"`
	RequestTimeout    time.Duration   `mapstructure:"requestTimeout"`
	ScriptTimeout     time.Duration   `mapstructure:"scriptTimeout"`
	MaxBodyBytes      int64           `mapstructure:"maxBodyBytes"`
	MaxConcurrentRuns int             `mapstructure:"maxConcurrentRuns"`
	Webhooks          []WebhookConfig `mapstructure:"webhooks"`
}
type WebhookConfig struct {
	Name            string   `mapstructure:"name"`
	Secret          string   `mapstructure:"secret"`
	Command         string   `mapstructure:"command"`
	Args            []string `mapstructure:"args"`
	Enabled         bool     `mapstructure:"enabled"`
	NotifyOnSuccess bool     `mapstructure:"notifyOnSuccess"`
	NotifyOnFailure bool     `mapstructure:"notifyOnFailure"`
}

var webhookNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func Load(configPath string) (*Config, error) {
	viper.Reset()
	viper.SetConfigType("toml")
	if configPath == "" {
		configPath = os.Getenv("CONFIG_PATH")
	}
	if configPath != "" {
		viper.SetConfigFile(configPath)
	}
	viper.SetDefault("app.name", "MaidCafe")
	viper.SetDefault("http.port", "8080")
	viper.SetDefault("database.dsn", "")
	viper.SetDefault("auth.target", "")
	viper.SetDefault("auth.useTLS", true)
	viper.SetDefault("auth.tlsSkipVerify", false)
	viper.SetDefault("eventbus.url", "")
	viper.SetDefault("ring.target", "")
	viper.SetDefault("ring.useTLS", false)
	viper.SetDefault("ring.tlsSkipVerify", false)
	viper.SetDefault("daemon.listen", "127.0.0.1:8747")
	viper.SetDefault("daemon.cloudUrl", "https://mk.solsynth.dev")
	viper.SetDefault("daemon.cloudSecret", "")
	viper.SetDefault("daemon.metricsInterval", time.Minute)
	viper.SetDefault("daemon.requestTimeout", 10*time.Second)
	viper.SetDefault("daemon.scriptTimeout", 30*time.Second)
	viper.SetDefault("daemon.maxBodyBytes", int64(65536))
	viper.SetDefault("daemon.maxConcurrentRuns", 4)
	applyEnvAliases()
	if configPath != "" {
		if err := viper.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
	}
	applyEnvAliases()
	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	return &cfg, nil
}

func applyEnvAliases() {
	aliases := map[string]string{
		"AUTH_TARGET": "auth.target", "AUTH_USE_TLS": "auth.useTLS", "AUTH_TLS_SKIP_VERIFY": "auth.tlsSkipVerify",
		"RING_TARGET": "ring.target", "RING_USE_TLS": "ring.useTLS", "RING_TLS_SKIP_VERIFY": "ring.tlsSkipVerify",
		"EVENTBUS_URL": "eventbus.url", "DAEMON_ID": "daemon.id", "DAEMON_LISTEN": "daemon.listen",
		"DAEMON_CLOUD_URL": "daemon.cloudUrl", "DAEMON_CLOUD_SECRET": "daemon.cloudSecret",
		"DAEMON_METRICS_INTERVAL": "daemon.metricsInterval", "DAEMON_REQUEST_TIMEOUT": "daemon.requestTimeout",
		"DAEMON_SCRIPT_TIMEOUT": "daemon.scriptTimeout", "DAEMON_MAX_BODY_BYTES": "daemon.maxBodyBytes",
		"DAEMON_MAX_CONCURRENT_RUNS": "daemon.maxConcurrentRuns",
	}
	for env, key := range aliases {
		if value, ok := os.LookupEnv(env); ok {
			viper.Set(key, value)
		}
	}
}

func (c *Config) ValidateCloud() error {
	if strings.TrimSpace(c.Database.DSN) == "" {
		return fmt.Errorf("database.dsn is required in cloud mode")
	}
	if strings.TrimSpace(c.Auth.Target) == "" {
		return fmt.Errorf("auth.target is required in cloud mode")
	}
	if err := validatePort(c.HTTP.Port); err != nil {
		return fmt.Errorf("http.port: %w", err)
	}
	return nil
}

func (c *Config) ValidateDaemon() error {
	if strings.TrimSpace(c.Daemon.ID) == "" {
		return fmt.Errorf("daemon.id is required in daemon mode")
	}
	if err := validateListen(c.Daemon.Listen); err != nil {
		return fmt.Errorf("daemon.listen: %w", err)
	}
	if c.Daemon.MetricsInterval <= 0 {
		return fmt.Errorf("daemon.metricsInterval must be positive")
	}
	if c.Daemon.RequestTimeout <= 0 {
		return fmt.Errorf("daemon.requestTimeout must be positive")
	}
	if c.Daemon.ScriptTimeout <= 0 {
		return fmt.Errorf("daemon.scriptTimeout must be positive")
	}
	if c.Daemon.MaxBodyBytes <= 0 {
		return fmt.Errorf("daemon.maxBodyBytes must be positive")
	}
	if c.Daemon.MaxConcurrentRuns <= 0 {
		return fmt.Errorf("daemon.maxConcurrentRuns must be positive")
	}
	seen := make(map[string]struct{}, len(c.Daemon.Webhooks))
	for i, hook := range c.Daemon.Webhooks {
		if strings.TrimSpace(hook.Name) == "" || !webhookNamePattern.MatchString(hook.Name) {
			return fmt.Errorf("daemon.webhooks[%d].name must match [A-Za-z0-9._-]+", i)
		}
		if _, ok := seen[hook.Name]; ok {
			return fmt.Errorf("daemon.webhooks[%d].name %q is duplicated", i, hook.Name)
		}
		seen[hook.Name] = struct{}{}
		if strings.TrimSpace(hook.Secret) == "" {
			return fmt.Errorf("daemon.webhooks[%d].secret is required", i)
		}
		if !filepath.IsAbs(hook.Command) {
			return fmt.Errorf("daemon.webhooks[%d].command must be an absolute path", i)
		}
	}
	if strings.TrimSpace(c.Daemon.CloudURL) != "" {
		u, err := url.Parse(c.Daemon.CloudURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"))) {
			return fmt.Errorf("daemon.cloudUrl must be HTTPS, or HTTP to localhost")
		}
	}
	return nil
}

func validateListen(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("must not be empty")
	}
	if _, err := net.ResolveTCPAddr("tcp", value); err != nil {
		return fmt.Errorf("invalid address: %w", err)
	}
	return nil
}
func validatePort(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("must not be empty")
	}
	if _, err := net.ResolveTCPAddr("tcp", ":"+strings.TrimPrefix(value, ":")); err != nil {
		return fmt.Errorf("invalid port: %w", err)
	}
	return nil
}
