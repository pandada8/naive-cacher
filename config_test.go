package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigDefaultsAndOverrides(test *testing.T) {
	configPath := writeTestConfig(test, `
listen: ":9090"
sites:
  - hosts: ["go.example.internal"]
    upstream: https://proxy.golang.org
    timeout: 45s
    cache_control: public, max-age=60
    paths:
      - regex: '\.zip$'
        timeout: 5m
        cache_control: public, max-age=315360000, immutable
`)
	cfg, err := LoadConfig(configPath)
	if err != nil {
		test.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.Listen != ":9090" {
		test.Fatalf("listen = %q, want :9090", cfg.Listen)
	}
	if cfg.ReadHeaderTimeout.Duration != 5*time.Second {
		test.Fatalf("read_header_timeout = %s, want 5s", cfg.ReadHeaderTimeout.Duration)
	}
	site := cfg.Sites[0]
	if site.Timeout.Duration != 45*time.Second {
		test.Fatalf("site timeout = %s, want 45s", site.Timeout.Duration)
	}
	if site.Paths[0].Timeout.Duration != 5*time.Minute {
		test.Fatalf("path timeout = %s, want 5m", site.Paths[0].Timeout.Duration)
	}
	if site.FollowRedirects != nil {
		test.Fatalf("follow_redirects should remain unset, got %v", *site.FollowRedirects)
	}
}

func TestLoadConfigRejectsDuplicateHost(test *testing.T) {
	configPath := writeTestConfig(test, `
sites:
  - hosts: ["go.example.internal"]
    upstream: https://proxy.golang.org
  - hosts: ["GO.example.internal:443"]
    upstream: https://example.com
`)
	if _, err := LoadConfig(configPath); err == nil {
		test.Fatal("expected duplicate host error")
	}
}

func TestLoadConfigRejectsInvalidPathRule(test *testing.T) {
	configPath := writeTestConfig(test, `
sites:
  - hosts: ["go.example.internal"]
    upstream: https://proxy.golang.org
    paths:
      - prefix: /@v/
        regex: '\.zip$'
`)
	if _, err := LoadConfig(configPath); err == nil {
		test.Fatal("expected invalid path rule error")
	}
}

func TestLoadConfigRejectsUnknownField(test *testing.T) {
	configPath := writeTestConfig(test, `
sites:
  - hosts: ["go.example.internal"]
    upstream: https://proxy.golang.org
    timout: 30s
`)
	if _, err := LoadConfig(configPath); err == nil {
		test.Fatal("expected unknown field error")
	}
}

func TestNormalizeHost(test *testing.T) {
	cases := map[string]string{
		"GO.Example.Internal:443": "go.example.internal",
		"go.example.internal.":    "go.example.internal",
		"[::1]:8080":              "::1",
		"*.Example.Internal":      "*.example.internal",
	}
	for input, expected := range cases {
		if actual := normalizeHost(input); actual != expected {
			test.Fatalf("normalizeHost(%q) = %q, want %q", input, actual, expected)
		}
	}
}

func writeTestConfig(test *testing.T, content string) string {
	test.Helper()
	configPath := filepath.Join(test.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		test.Fatalf("write config: %v", err)
	}
	return configPath
}

func stringPointer(value string) *string {
	return &value
}

func boolPointer(value bool) *bool {
	return &value
}

func intPointer(value int) *int {
	return &value
}

func durationPointer(value time.Duration) *Duration {
	return &Duration{value}
}
