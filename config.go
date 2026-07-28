package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/BurntSushi/toml"
)

// Config holds server configuration.
type Config struct {
	Mode             string // "root" or "proxy" (default: "proxy")
	UpstreamURL      string // upstream hub URL, only used in proxy mode
	UpstreamPush     string // "public" (default) or "all" — controls what gets forwarded upstream
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

	Freenet FreenetConfig // write-through Freenet mirror (off by default)
}

// FreenetConfig configures the write-through Freenet mirror. Disabled by
// default: with Enabled false the hub behaves exactly as it did before this
// feature existed, and nothing is ever handed to an external command.
type FreenetConfig struct {
	Enabled    bool
	PublishCmd string        // absolute path to the publish script (e.g. publish-v2.sh)
	QueueDir   string        // defaults to <store_dir>/freenet-queue
	Timeout    time.Duration // per-publish wall-clock budget (default: 15m)
	Retries    int           // retry attempts after the initial one (default: 3)
}

// fileConfig mirrors Config but with pointer fields so we can distinguish
// "not set in TOML" from "set to zero value".
type fileConfig struct {
	Mode             *string `toml:"mode"`
	UpstreamURL      *string `toml:"upstream_url"`
	UpstreamPush     *string `toml:"upstream_push"`
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

	Realms  map[string]realmConfig `toml:"realms"`
	Freenet *freenetFileConfig     `toml:"freenet"`
}

// realmConfig holds the config for a single shared realm.
type realmConfig struct {
	Members []string `toml:"members"`
}

// freenetFileConfig is the [freenet] TOML section.
type freenetFileConfig struct {
	Enabled    *bool   `toml:"enabled"`
	PublishCmd *string `toml:"publish_cmd"`
	QueueDir   *string `toml:"queue_dir"`
	Timeout    *string `toml:"timeout"`
	Retries    *int    `toml:"retries"`
}

// loadConfig builds the final Config by layering: defaults < TOML file < env vars.
// Returns the config and the config file path (empty if none provided).
func loadConfig() (Config, string) {
	configPath := flag.String("config", "", "path to TOML config file")
	flag.Parse()

	// 1. Defaults
	cfg := defaultConfig()

	// 2. TOML file (if provided)
	if *configPath != "" {
		if err := applyFile(&cfg, *configPath); err != nil {
			log.Fatalf("Failed to load config file %s: %v", *configPath, err)
		}
		log.Printf("Loaded config from %s", *configPath)
	}

	// 3. Env vars override
	applyEnv(&cfg)

	// 4. Defaults that depend on other settled values
	resolveFreenetDefaults(&cfg)

	return cfg, *configPath
}

// defaultConfig returns the built-in defaults, before file and env layering.
func defaultConfig() Config {
	return Config{
		Mode:             "proxy",
		UpstreamURL:      "https://dataverse001.net",
		UpstreamPush:     "public",
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
		Freenet: FreenetConfig{
			Enabled: false, // explicit: the mirror never turns itself on
			Timeout: 15 * time.Minute,
			Retries: 3,
		},
	}
}

// resolveFreenetDefaults fills in the queue dir, which defaults to a
// subdirectory of the store dir and so can only be settled once store_dir is
// final.
func resolveFreenetDefaults(cfg *Config) {
	if cfg.Freenet.QueueDir == "" {
		cfg.Freenet.QueueDir = filepath.Join(cfg.StoreDir, "freenet-queue")
	}
}

// loadRealmsFromFile parses the TOML config and returns the shared realm map.
// Returns nil map (not error) if no realms are configured.
func loadRealmsFromFile(path string) (map[string][]string, error) {
	if path == "" {
		return nil, nil
	}
	var fc fileConfig
	if _, err := toml.DecodeFile(path, &fc); err != nil {
		return nil, fmt.Errorf("parsing TOML: %w", err)
	}
	if len(fc.Realms) == 0 {
		return nil, nil
	}
	result := make(map[string][]string, len(fc.Realms))
	for name, rc := range fc.Realms {
		result[name] = rc.Members
	}
	return result, nil
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
	if fc.UpstreamPush != nil {
		cfg.UpstreamPush = *fc.UpstreamPush
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
		cfg.AuthTokenExpiry = parseDurationOr("auth_token_expiry", *fc.AuthTokenExpiry, cfg.AuthTokenExpiry)
	}
	if fc.BaseDomain != nil {
		cfg.BaseDomain = *fc.BaseDomain
	}
	if fc.VhostMode != nil {
		cfg.VhostMode = *fc.VhostMode
	}
	if fc.TxtCacheTTL != nil {
		cfg.TxtCacheTTL = parseDurationOr("txt_cache_ttl", *fc.TxtCacheTTL, cfg.TxtCacheTTL)
	}
	if fn := fc.Freenet; fn != nil {
		if fn.Enabled != nil {
			cfg.Freenet.Enabled = *fn.Enabled
		}
		if fn.PublishCmd != nil {
			cfg.Freenet.PublishCmd = *fn.PublishCmd
		}
		if fn.QueueDir != nil {
			cfg.Freenet.QueueDir = *fn.QueueDir
		}
		if fn.Timeout != nil {
			cfg.Freenet.Timeout = parseDurationOr("freenet.timeout", *fn.Timeout, cfg.Freenet.Timeout)
		}
		if fn.Retries != nil {
			cfg.Freenet.Retries = *fn.Retries
		}
	}

	return nil
}

// parseDurationOr parses a Go duration string, keeping (and warning about)
// the current value if it is malformed.
func parseDurationOr(name, value string, current time.Duration) time.Duration {
	d, err := time.ParseDuration(value)
	if err != nil {
		log.Printf("WARN: invalid %s=%q, keeping %v", name, value, current)
		return current
	}
	return d
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("DATAVERSE_MODE"); v != "" {
		cfg.Mode = v
	}
	if v := os.Getenv("DATAVERSE_UPSTREAM_URL"); v != "" {
		cfg.UpstreamURL = v
	}
	if v := os.Getenv("DATAVERSE_UPSTREAM_PUSH"); v != "" {
		cfg.UpstreamPush = v
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
		cfg.AuthTokenExpiry = parseDurationOr("HUB_AUTH_TOKEN_EXPIRY", v, cfg.AuthTokenExpiry)
	}
	if v := os.Getenv("HUB_BASE_DOMAIN"); v != "" {
		cfg.BaseDomain = v
	}
	if v := os.Getenv("HUB_VHOST_MODE"); v != "" {
		cfg.VhostMode = v
	}
	if v := os.Getenv("HUB_TXT_CACHE_TTL"); v != "" {
		cfg.TxtCacheTTL = parseDurationOr("HUB_TXT_CACHE_TTL", v, cfg.TxtCacheTTL)
	}
	if v := os.Getenv("HUB_FREENET_ENABLED"); v != "" {
		cfg.Freenet.Enabled = v == "true"
	}
	if v := os.Getenv("HUB_FREENET_PUBLISH_CMD"); v != "" {
		cfg.Freenet.PublishCmd = v
	}
	if v := os.Getenv("HUB_FREENET_QUEUE_DIR"); v != "" {
		cfg.Freenet.QueueDir = v
	}
	if v := os.Getenv("HUB_FREENET_TIMEOUT"); v != "" {
		cfg.Freenet.Timeout = parseDurationOr("HUB_FREENET_TIMEOUT", v, cfg.Freenet.Timeout)
	}
	if v := os.Getenv("HUB_FREENET_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Freenet.Retries = n
		} else {
			log.Printf("WARN: invalid HUB_FREENET_RETRIES=%q, keeping %d", v, cfg.Freenet.Retries)
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

	switch cfg.UpstreamPush {
	case "", "public":
		cfg.UpstreamPush = "public"
	case "all":
		// valid
	default:
		log.Printf("WARN: invalid upstream_push=%q, using %q", cfg.UpstreamPush, "public")
		cfg.UpstreamPush = "public"
	}
}
