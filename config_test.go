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

func TestFreenetConfigFromFile(t *testing.T) {
	tomlContent := `
store_dir = "/data/dv"

[freenet]
enabled = true
publish_cmd = "/opt/freenet/publish-v2.sh"
queue_dir = "/var/lib/dv/freenet"
timeout = "20m"
retries = 5
`
	path := filepath.Join(t.TempDir(), "hub.toml")
	if err := os.WriteFile(path, []byte(tomlContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := defaultConfig()
	if err := applyFile(&cfg, path); err != nil {
		t.Fatalf("applyFile: %v", err)
	}

	if !cfg.Freenet.Enabled {
		t.Error("Freenet.Enabled = false, want true")
	}
	if cfg.Freenet.PublishCmd != "/opt/freenet/publish-v2.sh" {
		t.Errorf("Freenet.PublishCmd = %q", cfg.Freenet.PublishCmd)
	}
	if cfg.Freenet.QueueDir != "/var/lib/dv/freenet" {
		t.Errorf("Freenet.QueueDir = %q", cfg.Freenet.QueueDir)
	}
	if cfg.Freenet.Timeout != 20*time.Minute {
		t.Errorf("Freenet.Timeout = %v, want 20m", cfg.Freenet.Timeout)
	}
	if cfg.Freenet.Retries != 5 {
		t.Errorf("Freenet.Retries = %d, want 5", cfg.Freenet.Retries)
	}
}

// The whole feature must be inert unless it is explicitly switched on.
func TestFreenetDisabledByDefault(t *testing.T) {
	cfg := defaultConfig()
	if cfg.Freenet.Enabled {
		t.Error("Freenet.Enabled = true by default, want false")
	}
	if cfg.Freenet.PublishCmd != "" {
		t.Errorf("Freenet.PublishCmd = %q by default, want empty", cfg.Freenet.PublishCmd)
	}
	if cfg.Freenet.Timeout != 15*time.Minute {
		t.Errorf("Freenet.Timeout = %v, want the 15m default", cfg.Freenet.Timeout)
	}
	if cfg.Freenet.Retries != 3 {
		t.Errorf("Freenet.Retries = %d, want the default 3", cfg.Freenet.Retries)
	}
}

// A config file with no [freenet] section must leave the defaults alone.
func TestFreenetAbsentSectionKeepsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.toml")
	if err := os.WriteFile(path, []byte("mode = \"root\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := defaultConfig()
	if err := applyFile(&cfg, path); err != nil {
		t.Fatalf("applyFile: %v", err)
	}
	if cfg.Freenet.Enabled {
		t.Error("Freenet.Enabled = true with no [freenet] section, want false")
	}
	if cfg.Freenet.Retries != 3 || cfg.Freenet.Timeout != 15*time.Minute {
		t.Errorf("Freenet defaults disturbed: %+v", cfg.Freenet)
	}
}

func TestFreenetConfigFromEnv(t *testing.T) {
	cfg := defaultConfig()

	t.Setenv("HUB_FREENET_ENABLED", "true")
	t.Setenv("HUB_FREENET_PUBLISH_CMD", "/srv/publish-v2.sh")
	t.Setenv("HUB_FREENET_QUEUE_DIR", "/srv/queue")
	t.Setenv("HUB_FREENET_TIMEOUT", "90s")
	t.Setenv("HUB_FREENET_RETRIES", "1")
	applyEnv(&cfg)

	if !cfg.Freenet.Enabled {
		t.Error("Freenet.Enabled = false, want true (env)")
	}
	if cfg.Freenet.PublishCmd != "/srv/publish-v2.sh" {
		t.Errorf("Freenet.PublishCmd = %q", cfg.Freenet.PublishCmd)
	}
	if cfg.Freenet.QueueDir != "/srv/queue" {
		t.Errorf("Freenet.QueueDir = %q", cfg.Freenet.QueueDir)
	}
	if cfg.Freenet.Timeout != 90*time.Second {
		t.Errorf("Freenet.Timeout = %v, want 90s", cfg.Freenet.Timeout)
	}
	if cfg.Freenet.Retries != 1 {
		t.Errorf("Freenet.Retries = %d, want 1", cfg.Freenet.Retries)
	}
}

func TestFreenetInvalidEnvKeepsPrevious(t *testing.T) {
	cfg := defaultConfig()
	t.Setenv("HUB_FREENET_TIMEOUT", "not-a-duration")
	t.Setenv("HUB_FREENET_RETRIES", "lots")
	applyEnv(&cfg)

	if cfg.Freenet.Timeout != 15*time.Minute {
		t.Errorf("Freenet.Timeout = %v, want the default kept after invalid env", cfg.Freenet.Timeout)
	}
	if cfg.Freenet.Retries != 3 {
		t.Errorf("Freenet.Retries = %d, want the default kept after invalid env", cfg.Freenet.Retries)
	}
}

func TestFreenetQueueDirDefaultsUnderStoreDir(t *testing.T) {
	cfg := defaultConfig()
	cfg.StoreDir = "/data/dv"
	resolveFreenetDefaults(&cfg)

	want := filepath.Join("/data/dv", "freenet-queue")
	if cfg.Freenet.QueueDir != want {
		t.Errorf("Freenet.QueueDir = %q, want %q", cfg.Freenet.QueueDir, want)
	}
}

func TestFreenetExplicitQueueDirWins(t *testing.T) {
	cfg := defaultConfig()
	cfg.StoreDir = "/data/dv"
	cfg.Freenet.QueueDir = "/elsewhere/queue"
	resolveFreenetDefaults(&cfg)

	if cfg.Freenet.QueueDir != "/elsewhere/queue" {
		t.Errorf("Freenet.QueueDir = %q, want the explicit value", cfg.Freenet.QueueDir)
	}
}
