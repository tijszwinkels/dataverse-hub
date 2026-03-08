package main

import (
	"os"
	"path/filepath"
	"testing"
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
