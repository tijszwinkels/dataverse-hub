package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/BurntSushi/toml"
)

// Config holds server configuration.
type Config struct {
	Mode             string // "root" or "proxy" (default: "proxy")
	UpstreamURL      string // upstream hub URL, only used in proxy mode
	Addr             string
	StoreDir         string
	RateLimitPerMin  int
	RateLimitPerDay  int
	DefaultViewerRef string // PAGE ref to use as default object viewer for browsers
	BackupEnabled    bool   // keep old revisions in bk/ (default: true)

	AuthTokenExpiry time.Duration // bearer token lifetime (default: 168h = 7 days)

	BaseDomain  string        // e.g. "dataverse001.net", required for "redirect" and "isolate"
	VhostMode   string        // "off", "redirect", or "isolate"
	TxtCacheTTL time.Duration // TXT record cache TTL (default: 5m)
}

// fileConfig mirrors Config but with pointer fields so we can distinguish
// "not set in TOML" from "set to zero value".
type fileConfig struct {
	Mode             *string `toml:"mode"`
	UpstreamURL      *string `toml:"upstream_url"`
	Addr             *string `toml:"addr"`
	StoreDir         *string `toml:"store_dir"`
	RateLimitPerMin  *int    `toml:"rate_limit_per_min"`
	RateLimitPerDay  *int    `toml:"rate_limit_per_day"`
	DefaultViewerRef *string `toml:"default_viewer_ref"`
	BackupEnabled    *bool   `toml:"backup_enabled"`
	AuthTokenExpiry  *string `toml:"auth_token_expiry"`
	BaseDomain       *string `toml:"base_domain"`
	VhostMode        *string `toml:"vhost_mode"`
	TxtCacheTTL      *string `toml:"txt_cache_ttl"`
}

// loadConfig builds the final Config by layering: defaults < TOML file < env vars.
func loadConfig() Config {
	configPath := flag.String("config", "", "path to TOML config file")
	flag.Parse()

	// 1. Defaults
	cfg := Config{
		Mode:             "proxy",
		UpstreamURL:      "https://dataverse001.net",
		Addr:             ":5678",
		StoreDir:         "./dataverse001",
		RateLimitPerMin:  120,
		RateLimitPerDay:  20000,
		DefaultViewerRef: "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.b3f5a7c9-2d4e-4f60-9b8a-0c1d2e3f4a5b",
		BackupEnabled:    true,
		AuthTokenExpiry:  168 * time.Hour, // 7 days
		BaseDomain:       "localhost",
		VhostMode:        "isolate",
		TxtCacheTTL:      5 * time.Minute,
	}

	// 2. TOML file (if provided)
	if *configPath != "" {
		if err := applyFile(&cfg, *configPath); err != nil {
			log.Fatalf("Failed to load config file %s: %v", *configPath, err)
		}
		log.Printf("Loaded config from %s", *configPath)
	}

	// 3. Env vars override
	applyEnv(&cfg)

	return cfg
}

func applyFile(cfg *Config, path string) error {
	var fc fileConfig
	if _, err := toml.DecodeFile(path, &fc); err != nil {
		return fmt.Errorf("parsing TOML: %w", err)
	}

	if fc.Mode != nil {
		cfg.Mode = *fc.Mode
	}
	if fc.UpstreamURL != nil {
		cfg.UpstreamURL = *fc.UpstreamURL
	}
	if fc.Addr != nil {
		cfg.Addr = *fc.Addr
	}
	if fc.StoreDir != nil {
		cfg.StoreDir = *fc.StoreDir
	}
	if fc.RateLimitPerMin != nil {
		cfg.RateLimitPerMin = *fc.RateLimitPerMin
	}
	if fc.RateLimitPerDay != nil {
		cfg.RateLimitPerDay = *fc.RateLimitPerDay
	}
	if fc.DefaultViewerRef != nil {
		cfg.DefaultViewerRef = *fc.DefaultViewerRef
	}
	if fc.BackupEnabled != nil {
		cfg.BackupEnabled = *fc.BackupEnabled
	}
	if fc.AuthTokenExpiry != nil {
		if d, err := time.ParseDuration(*fc.AuthTokenExpiry); err == nil {
			cfg.AuthTokenExpiry = d
		} else {
			log.Printf("WARN: invalid auth_token_expiry=%q, keeping %v", *fc.AuthTokenExpiry, cfg.AuthTokenExpiry)
		}
	}
	if fc.BaseDomain != nil {
		cfg.BaseDomain = *fc.BaseDomain
	}
	if fc.VhostMode != nil {
		cfg.VhostMode = *fc.VhostMode
	}
	if fc.TxtCacheTTL != nil {
		if d, err := time.ParseDuration(*fc.TxtCacheTTL); err == nil {
			cfg.TxtCacheTTL = d
		} else {
			log.Printf("WARN: invalid txt_cache_ttl=%q, keeping %v", *fc.TxtCacheTTL, cfg.TxtCacheTTL)
		}
	}

	return nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("DATAVERSE_MODE"); v != "" {
		cfg.Mode = v
	}
	if v := os.Getenv("DATAVERSE_UPSTREAM_URL"); v != "" {
		cfg.UpstreamURL = v
	}
	if v := os.Getenv("HUB_ADDR"); v != "" {
		cfg.Addr = v
	}
	if v := os.Getenv("HUB_STORE_DIR"); v != "" {
		cfg.StoreDir = v
	}
	if v := os.Getenv("HUB_RATE_LIMIT_PER_MIN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.RateLimitPerMin = n
		} else {
			log.Printf("WARN: invalid HUB_RATE_LIMIT_PER_MIN=%q, keeping %d", v, cfg.RateLimitPerMin)
		}
	}
	if v := os.Getenv("HUB_RATE_LIMIT_PER_DAY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.RateLimitPerDay = n
		} else {
			log.Printf("WARN: invalid HUB_RATE_LIMIT_PER_DAY=%q, keeping %d", v, cfg.RateLimitPerDay)
		}
	}
	if v := os.Getenv("HUB_DEFAULT_VIEWER_REF"); v != "" {
		cfg.DefaultViewerRef = v
	}
	if v := os.Getenv("HUB_BACKUP_ENABLED"); v != "" {
		cfg.BackupEnabled = v == "true"
	}
	if v := os.Getenv("HUB_AUTH_TOKEN_EXPIRY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.AuthTokenExpiry = d
		} else {
			log.Printf("WARN: invalid HUB_AUTH_TOKEN_EXPIRY=%q, keeping %v", v, cfg.AuthTokenExpiry)
		}
	}
	if v := os.Getenv("HUB_BASE_DOMAIN"); v != "" {
		cfg.BaseDomain = v
	}
	if v := os.Getenv("HUB_VHOST_MODE"); v != "" {
		cfg.VhostMode = v
	}
	if v := os.Getenv("HUB_TXT_CACHE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.TxtCacheTTL = d
		} else {
			log.Printf("WARN: invalid HUB_TXT_CACHE_TTL=%q, keeping %v", v, cfg.TxtCacheTTL)
		}
	}

	switch cfg.VhostMode {
	case "", "isolate", "redirect", "off":
		if cfg.VhostMode == "" {
			cfg.VhostMode = "isolate"
		}
	default:
		log.Printf("WARN: invalid HUB_VHOST_MODE=%q, keeping %q", cfg.VhostMode, "isolate")
		cfg.VhostMode = "isolate"
	}
}
