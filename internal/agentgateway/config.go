package agentgateway

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const Version = "1.3.0"

type Config struct {
	ListenAddress   string
	UpstreamURL     *url.URL
	AuditFile       string
	AuditKey        []byte
	MetricsFile     string
	MetricsToken    string
	MaxBodyBytes    int64
	MaxResponse     int64
	RatePerMinute   int
	RateBurst       int
	UpstreamTimeout time.Duration
	ReadyTimeout    time.Duration
	SLOP95Target    time.Duration
	SLOAvailability float64
	SLOWindow       time.Duration
}

func ConfigFromEnvironment() (Config, error) {
	upstream, err := url.Parse(envString("CHANGEGUARD_AGENT_GATEWAY_UPSTREAM", "http://127.0.0.1:8080"))
	if err != nil {
		return Config{}, fmt.Errorf("parse upstream URL: %w", err)
	}
	auditKey, err := decodeAuditKey(strings.TrimSpace(os.Getenv("CHANGEGUARD_AGENT_AUDIT_KEY")))
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		ListenAddress:   envString("CHANGEGUARD_AGENT_GATEWAY_LISTEN", "127.0.0.1:18081"),
		UpstreamURL:     upstream,
		AuditFile:       envString("CHANGEGUARD_AGENT_AUDIT_FILE", "/opt/changeguard-agent/data/audit.jsonl"),
		AuditKey:        auditKey,
		MetricsFile:     envString("CHANGEGUARD_AGENT_METRICS_FILE", "/opt/changeguard-agent/data/metrics.json"),
		MetricsToken:    strings.TrimSpace(os.Getenv("CHANGEGUARD_AGENT_METRICS_TOKEN")),
		MaxBodyBytes:    int64(envInt("CHANGEGUARD_AGENT_MAX_BODY_BYTES", 128<<10)),
		MaxResponse:     int64(envInt("CHANGEGUARD_AGENT_MAX_RESPONSE_BYTES", 4<<20)),
		RatePerMinute:   envInt("CHANGEGUARD_AGENT_RATE_PER_MINUTE", 12),
		RateBurst:       envInt("CHANGEGUARD_AGENT_RATE_BURST", 4),
		UpstreamTimeout: envDuration("CHANGEGUARD_AGENT_UPSTREAM_TIMEOUT", 120*time.Second),
		ReadyTimeout:    envDuration("CHANGEGUARD_AGENT_READY_TIMEOUT", 2*time.Second),
		SLOP95Target:    envDuration("CHANGEGUARD_AGENT_SLO_P95_TARGET", 30*time.Second),
		SLOAvailability: envFloat("CHANGEGUARD_AGENT_SLO_AVAILABILITY_TARGET", 99.0),
		SLOWindow:       envDuration("CHANGEGUARD_AGENT_SLO_WINDOW", 24*time.Hour),
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.ListenAddress) == "" {
		return errors.New("listen address is required")
	}
	if c.UpstreamURL == nil || c.UpstreamURL.Scheme != "http" || c.UpstreamURL.Host == "" {
		return errors.New("upstream must be an absolute http URL")
	}
	host := strings.ToLower(c.UpstreamURL.Hostname())
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("upstream must resolve to a loopback address")
	}
	if len(c.AuditKey) < 32 {
		return errors.New("CHANGEGUARD_AGENT_AUDIT_KEY must contain at least 32 bytes")
	}
	if strings.TrimSpace(c.AuditFile) == "" {
		return errors.New("audit file is required")
	}
	if strings.TrimSpace(c.MetricsFile) == "" {
		return errors.New("metrics state file is required")
	}
	if c.MaxBodyBytes < 1024 || c.MaxBodyBytes > 1<<20 {
		return errors.New("max body bytes must be between 1 KiB and 1 MiB")
	}
	if c.MaxResponse < 64<<10 || c.MaxResponse > 16<<20 {
		return errors.New("max response bytes must be between 64 KiB and 16 MiB")
	}
	if c.RatePerMinute < 1 || c.RatePerMinute > 600 {
		return errors.New("rate per minute must be between 1 and 600")
	}
	if c.RateBurst < 1 || c.RateBurst > 100 {
		return errors.New("rate burst must be between 1 and 100")
	}
	if c.UpstreamTimeout < time.Second || c.UpstreamTimeout > 10*time.Minute {
		return errors.New("upstream timeout must be between 1 second and 10 minutes")
	}
	if c.SLOP95Target < time.Second || c.SLOP95Target > 10*time.Minute {
		return errors.New("SLO P95 target must be between 1 second and 10 minutes")
	}
	if c.SLOAvailability < 50 || c.SLOAvailability > 100 {
		return errors.New("SLO availability target must be between 50 and 100 percent")
	}
	if c.SLOWindow < time.Hour || c.SLOWindow > 31*24*time.Hour {
		return errors.New("SLO window must be between 1 hour and 31 days")
	}
	return nil
}

func decodeAuditKey(raw string) ([]byte, error) {
	if raw == "" {
		return nil, errors.New("CHANGEGUARD_AGENT_AUDIT_KEY is required")
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if decoded, err := encoding.DecodeString(raw); err == nil && len(decoded) >= 32 {
			return decoded, nil
		}
	}
	if len(raw) >= 32 {
		return []byte(raw), nil
	}
	return nil, errors.New("CHANGEGUARD_AGENT_AUDIT_KEY must be base64 or raw text with at least 32 bytes")
}

func envString(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envFloat(key string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
