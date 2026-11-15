package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration struct {
	time.Duration
}

func (duration *Duration) UnmarshalYAML(value *yaml.Node) error {
	var raw string
	if err := value.Decode(&raw); err != nil {
		return fmt.Errorf("duration must be a string such as 30s or 5m: %w", err)
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	duration.Duration = parsed
	return nil
}

type Config struct {
	Listen            string       `yaml:"listen"`
	LogLevel          string       `yaml:"log_level"`
	ReadHeaderTimeout *Duration    `yaml:"read_header_timeout"`
	IdleTimeout       *Duration    `yaml:"idle_timeout"`
	ShutdownTimeout   *Duration    `yaml:"shutdown_timeout"`
	Sites             []SiteConfig `yaml:"sites"`
}

type SiteConfig struct {
	Hosts           []string         `yaml:"hosts"`
	Upstream        string           `yaml:"upstream"`
	FollowRedirects *bool            `yaml:"follow_redirects"`
	MaxRedirects    *int             `yaml:"max_redirects"`
	Timeout         *Duration        `yaml:"timeout"`
	CacheControl    *string          `yaml:"cache_control"`
	Paths           []PathRuleConfig `yaml:"paths"`
}

type PathRuleConfig struct {
	Prefix       string    `yaml:"prefix"`
	Regex        string    `yaml:"regex"`
	Timeout      *Duration `yaml:"timeout"`
	CacheControl *string   `yaml:"cache_control"`
}

func DefaultConfig() Config {
	return Config{
		Listen:            ":8080",
		LogLevel:          "info",
		ReadHeaderTimeout: &Duration{5 * time.Second},
		IdleTimeout:       &Duration{90 * time.Second},
		ShutdownTimeout:   &Duration{10 * time.Second},
	}
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := DefaultConfig()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return &cfg, nil
}

func (cfg *Config) applyDefaults() {
	defaults := DefaultConfig()
	if cfg.Listen == "" {
		cfg.Listen = defaults.Listen
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = defaults.LogLevel
	}
	if cfg.ReadHeaderTimeout == nil {
		cfg.ReadHeaderTimeout = defaults.ReadHeaderTimeout
	}
	if cfg.IdleTimeout == nil {
		cfg.IdleTimeout = defaults.IdleTimeout
	}
	if cfg.ShutdownTimeout == nil {
		cfg.ShutdownTimeout = defaults.ShutdownTimeout
	}
}

func (cfg *Config) Validate() error {
	cfg.applyDefaults()
	if strings.TrimSpace(cfg.Listen) == "" {
		return fmt.Errorf("listen must not be empty")
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("invalid log_level %q: %w", cfg.LogLevel, err)
	}
	if cfg.ReadHeaderTimeout.Duration < 0 {
		return fmt.Errorf("read_header_timeout must not be negative")
	}
	if cfg.IdleTimeout.Duration < 0 {
		return fmt.Errorf("idle_timeout must not be negative")
	}
	if cfg.ShutdownTimeout.Duration < 0 {
		return fmt.Errorf("shutdown_timeout must not be negative")
	}
	if len(cfg.Sites) == 0 {
		return fmt.Errorf("at least one site is required")
	}
	seenHosts := make(map[string]string)
	for siteIndex, site := range cfg.Sites {
		if len(site.Hosts) == 0 {
			return fmt.Errorf("site %d: hosts must not be empty", siteIndex)
		}
		for _, rawHost := range site.Hosts {
			host := normalizeHost(rawHost)
			if host == "" {
				return fmt.Errorf("site %d: host must not be empty", siteIndex)
			}
			if strings.ContainsAny(host, " \t\r\n/") {
				return fmt.Errorf("site %d: invalid host %q", siteIndex, rawHost)
			}
			if strings.Contains(host, "*") {
				if host != "*" && (!strings.HasPrefix(host, "*.") || len(host) == 2 || strings.Count(host, "*") != 1) {
					return fmt.Errorf("site %d: invalid wildcard host %q", siteIndex, rawHost)
				}
			}
			if previous, exists := seenHosts[host]; exists {
				return fmt.Errorf("site %d: host %q is already used by %s", siteIndex, rawHost, previous)
			}
			seenHosts[host] = fmt.Sprintf("site %d", siteIndex)
		}
		if _, err := parseUpstream(site.Upstream); err != nil {
			return fmt.Errorf("site %d: %w", siteIndex, err)
		}
		if site.MaxRedirects != nil && *site.MaxRedirects < 0 {
			return fmt.Errorf("site %d: max_redirects must not be negative", siteIndex)
		}
		if site.Timeout != nil && site.Timeout.Duration < 0 {
			return fmt.Errorf("site %d: timeout must not be negative", siteIndex)
		}
		for ruleIndex, rule := range site.Paths {
			matchers := 0
			if rule.Prefix != "" {
				matchers++
				if !strings.HasPrefix(rule.Prefix, "/") {
					return fmt.Errorf("site %d path %d: prefix must start with /", siteIndex, ruleIndex)
				}
			}
			if rule.Regex != "" {
				matchers++
				if _, err := regexp.Compile(rule.Regex); err != nil {
					return fmt.Errorf("site %d path %d: invalid regex: %w", siteIndex, ruleIndex, err)
				}
			}
			if matchers != 1 {
				return fmt.Errorf("site %d path %d: exactly one of prefix or regex is required", siteIndex, ruleIndex)
			}
			if rule.Timeout != nil && rule.Timeout.Duration < 0 {
				return fmt.Errorf("site %d path %d: timeout must not be negative", siteIndex, ruleIndex)
			}
		}
	}
	return nil
}

func parseUpstream(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("upstream must not be empty")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream %q: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("upstream %q must use http or https", raw)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("upstream %q must include a host", raw)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("upstream %q must not include userinfo", raw)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return nil, fmt.Errorf("upstream %q must not include query or fragment", raw)
	}
	return parsed, nil
}

func normalizeHost(raw string) string {
	host := strings.TrimSpace(raw)
	if splitHost, _, err := net.SplitHostPort(host); err == nil {
		host = splitHost
	}
	host = strings.ToLower(host)
	host = strings.TrimSuffix(host, ".")
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return host
}
