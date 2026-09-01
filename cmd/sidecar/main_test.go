package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigRejectsInvalidInterval(t *testing.T) {
	for _, value := range []string{"0s", "-1s", "not-a-duration"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("INTERVAL", value)
			if _, err := loadConfig(); err == nil {
				t.Fatalf("loadConfig accepted INTERVAL=%q", value)
			}
		})
	}
}

func TestLoadConfigAcceptsPositiveInterval(t *testing.T) {
	t.Setenv("INTERVAL", "45s")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Interval != 45*time.Second {
		t.Fatalf("Interval = %s, want 45s", cfg.Interval)
	}
}

func TestOpenCodeKeysFromEnvTrimsSecrets(t *testing.T) {
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "OPENCODE_GO_API_KEY") {
			t.Setenv(name, "")
		}
	}
	t.Setenv("OPENCODE_GO_API_KEY", "  main-secret\n")
	t.Setenv("OPENCODE_GO_API_KEY_A", "\ta-secret ")
	t.Setenv("OPENCODE_GO_API_KEY_EMPTY", " \n")
	t.Setenv("OPENCODE_GO_API_KEY_", "must-not-create-an-empty-label")

	keys := openCodeKeysFromEnv()
	if keys["Main"] != "main-secret" || keys["A"] != "a-secret" {
		t.Fatalf("keys = %#v, want trimmed Main and A", keys)
	}
	if len(keys) != 2 {
		t.Fatalf("keys = %#v, want only Main and A", keys)
	}
}

func TestURLForLogRemovesCredentials(t *testing.T) {
	got := urlForLog("https://admin:secret@example.test:8443/api")
	if strings.Contains(got, "admin") || strings.Contains(got, "secret") {
		t.Fatalf("urlForLog leaked credentials: %q", got)
	}
	if got != "https://example.test:8443/api" {
		t.Fatalf("urlForLog = %q", got)
	}
}

func TestLoadConfigRejectsUnsafeBifrostURL(t *testing.T) {
	for _, value := range []string{
		"admin:secret@example.test:8443/api",
		"ftp://example.test/api",
		"https://example.test/api?token=topsecret",
		"https://example.test/api#secret",
	} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("BIFROST_URL", value)
			if _, err := loadConfig(); err == nil {
				t.Fatalf("loadConfig accepted BIFROST_URL=%q", value)
			} else if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "token") {
				t.Fatalf("configuration error leaked credentials: %v", err)
			}
		})
	}
}

func TestURLForLogDropsQueryAndRejectsOpaqueValues(t *testing.T) {
	if got := urlForLog("https://example.test/api?token=topsecret#fragment"); got != "https://example.test/api" {
		t.Fatalf("urlForLog = %q, want URL without query or fragment", got)
	}
	if got := urlForLog("admin:secret@example.test:8443/api"); got != "<URL invalide>" {
		t.Fatalf("urlForLog opaque value = %q", got)
	}
}
