package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestApplyFile(t *testing.T) {
	tomlContent := `
mode = "root"
addr = ":9090"
store_dir = "/data/dv"
rate_limit_per_min = 60
rate_limit_per_day = 5000
backup_enabled = false
`
	path := filepath.Join(t.TempDir(), "hub.toml")
	if err := os.WriteFile(path, []byte(tomlContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		Mode:            "proxy",
		Addr:            ":5678",
		StoreDir:        "./dataverse001",
		RateLimitPerMin: 120,
		RateLimitPerDay: 20000,
		BackupEnabled:   true,
	}

	if err := applyFile(&cfg, path); err != nil {
		t.Fatalf("applyFile: %v", err)
	}

	if cfg.Mode != "root" {
		t.Errorf("Mode = %q, want %q", cfg.Mode, "root")
	}
	if cfg.Addr != ":9090" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, ":9090")
	}
	if cfg.StoreDir != "/data/dv" {
		t.Errorf("StoreDir = %q, want %q", cfg.StoreDir, "/data/dv")
	}
	if cfg.RateLimitPerMin != 60 {
		t.Errorf("RateLimitPerMin = %d, want %d", cfg.RateLimitPerMin, 60)
	}
	if cfg.RateLimitPerDay != 5000 {
		t.Errorf("RateLimitPerDay = %d, want %d", cfg.RateLimitPerDay, 5000)
	}
	if cfg.BackupEnabled {
		t.Errorf("BackupEnabled = true, want false")
	}
}

func TestApplyFilePartial(t *testing.T) {
	tomlContent := `
mode = "root"
addr = ":9090"
`
	path := filepath.Join(t.TempDir(), "hub.toml")
	if err := os.WriteFile(path, []byte(tomlContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		Mode:            "proxy",
		Addr:            ":5678",
		StoreDir:        "./dataverse001",
		RateLimitPerMin: 120,
		BackupEnabled:   true,
	}

	if err := applyFile(&cfg, path); err != nil {
		t.Fatalf("applyFile: %v", err)
	}

	if cfg.Mode != "root" {
		t.Errorf("Mode = %q, want %q", cfg.Mode, "root")
	}
	if cfg.Addr != ":9090" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, ":9090")
	}
	// Unset fields keep defaults
	if cfg.StoreDir != "./dataverse001" {
		t.Errorf("StoreDir = %q, want %q (default)", cfg.StoreDir, "./dataverse001")
	}
	if cfg.RateLimitPerMin != 120 {
		t.Errorf("RateLimitPerMin = %d, want %d (default)", cfg.RateLimitPerMin, 120)
	}
	if !cfg.BackupEnabled {
		t.Errorf("BackupEnabled = false, want true (default)")
	}
}

func TestApplyEnvOverridesFile(t *testing.T) {
	tomlContent := `
mode = "root"
addr = ":9090"
rate_limit_per_min = 60
`
	path := filepath.Join(t.TempDir(), "hub.toml")
	if err := os.WriteFile(path, []byte(tomlContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		Mode:            "proxy",
		Addr:            ":5678",
		RateLimitPerMin: 120,
	}

	if err := applyFile(&cfg, path); err != nil {
		t.Fatalf("applyFile: %v", err)
	}

	// Env var overrides TOML
	t.Setenv("DATAVERSE_MODE", "proxy")
	t.Setenv("HUB_RATE_LIMIT_PER_MIN", "200")
	applyEnv(&cfg)

	if cfg.Mode != "proxy" {
		t.Errorf("Mode = %q, want %q (env override)", cfg.Mode, "proxy")
	}
	if cfg.Addr != ":9090" {
		t.Errorf("Addr = %q, want %q (from file, no env override)", cfg.Addr, ":9090")
	}
	if cfg.RateLimitPerMin != 200 {
		t.Errorf("RateLimitPerMin = %d, want %d (env override)", cfg.RateLimitPerMin, 200)
	}
}

func TestApplyEnvInvalidInt(t *testing.T) {
	cfg := Config{RateLimitPerMin: 120}

	t.Setenv("HUB_RATE_LIMIT_PER_MIN", "notanumber")
	applyEnv(&cfg)

	if cfg.RateLimitPerMin != 120 {
		t.Errorf("RateLimitPerMin = %d, want %d (kept after invalid env)", cfg.RateLimitPerMin, 120)
	}
}

func TestApplyFileInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(path, []byte("not valid toml [[["), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{}
	if err := applyFile(&cfg, path); err == nil {
		t.Error("expected error for invalid TOML, got nil")
	}
}

func TestApplyFileMissing(t *testing.T) {
	cfg := Config{}
	if err := applyFile(&cfg, "/nonexistent/hub.toml"); err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

func TestUpstreamPushFromFile(t *testing.T) {
	tomlContent := `
upstream_push = "all"
`
	path := filepath.Join(t.TempDir(), "hub.toml")
	if err := os.WriteFile(path, []byte(tomlContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{UpstreamPush: "public"}
	if err := applyFile(&cfg, path); err != nil {
		t.Fatalf("applyFile: %v", err)
	}

	if cfg.UpstreamPush != "all" {
		t.Errorf("UpstreamPush = %q, want %q", cfg.UpstreamPush, "all")
	}
}

func TestUpstreamPushFromEnv(t *testing.T) {
	cfg := Config{UpstreamPush: "public"}
	t.Setenv("DATAVERSE_UPSTREAM_PUSH", "all")
	applyEnv(&cfg)

	if cfg.UpstreamPush != "all" {
		t.Errorf("UpstreamPush = %q, want %q (env override)", cfg.UpstreamPush, "all")
	}
}

func TestUpstreamPushInvalidFallsBackToPublic(t *testing.T) {
	cfg := Config{UpstreamPush: "bogus"}
	applyEnv(&cfg)

	if cfg.UpstreamPush != "public" {
		t.Errorf("UpstreamPush = %q, want %q (fallback from invalid)", cfg.UpstreamPush, "public")
	}
}

func TestEventsConfigFromFile(t *testing.T) {
	tomlContent := `
events_enabled = false
events_retention = "48h"
events_max_subscribers = 32
events_upstream = false
events_prefetch = true
`
	path := filepath.Join(t.TempDir(), "hub.toml")
	if err := os.WriteFile(path, []byte(tomlContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		EventsEnabled:        true,
		EventsRetention:      168 * time.Hour,
		EventsMaxSubscribers: 256,
		EventsUpstream:       true,
	}
	if err := applyFile(&cfg, path); err != nil {
		t.Fatalf("applyFile: %v", err)
	}

	if cfg.EventsEnabled {
		t.Errorf("EventsEnabled = true, want false")
	}
	if cfg.EventsRetention != 48*time.Hour {
		t.Errorf("EventsRetention = %v, want 48h", cfg.EventsRetention)
	}
	if cfg.EventsMaxSubscribers != 32 {
		t.Errorf("EventsMaxSubscribers = %d, want 32", cfg.EventsMaxSubscribers)
	}
	if cfg.EventsUpstream {
		t.Errorf("EventsUpstream = true, want false")
	}
	if !cfg.EventsPrefetch {
		t.Errorf("EventsPrefetch = false, want true")
	}
}

func TestEventsConfigFromEnv(t *testing.T) {
	cfg := Config{EventsEnabled: true, EventsRetention: 168 * time.Hour, EventsMaxSubscribers: 256, EventsUpstream: true}
	t.Setenv("HUB_EVENTS_ENABLED", "false")
	t.Setenv("HUB_EVENTS_RETENTION", "24h")
	t.Setenv("HUB_EVENTS_MAX_SUBSCRIBERS", "64")
	t.Setenv("HUB_EVENTS_UPSTREAM", "false")
	t.Setenv("HUB_EVENTS_PREFETCH", "true")
	applyEnv(&cfg)

	if cfg.EventsEnabled || cfg.EventsUpstream || !cfg.EventsPrefetch {
		t.Errorf("bool envs not applied: %+v", cfg)
	}
	if cfg.EventsRetention != 24*time.Hour {
		t.Errorf("EventsRetention = %v, want 24h", cfg.EventsRetention)
	}
	if cfg.EventsMaxSubscribers != 64 {
		t.Errorf("EventsMaxSubscribers = %d, want 64", cfg.EventsMaxSubscribers)
	}
}

func TestEventsConfigDefaults(t *testing.T) {
	// Defaults come from loadConfig's literal; assert the documented values
	// so a silent default change fails a test.
	cfg := defaultConfig()
	if !cfg.EventsEnabled {
		t.Errorf("events must be enabled by default")
	}
	if cfg.EventsRetention != 168*time.Hour {
		t.Errorf("EventsRetention default = %v, want 168h", cfg.EventsRetention)
	}
	if cfg.EventsMaxSubscribers != 256 {
		t.Errorf("EventsMaxSubscribers default = %d, want 256", cfg.EventsMaxSubscribers)
	}
	if !cfg.EventsUpstream {
		t.Errorf("EventsUpstream must default to true")
	}
	if cfg.EventsPrefetch {
		t.Errorf("EventsPrefetch must default to false (cache-on-demand)")
	}
}
