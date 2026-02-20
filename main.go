package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	cfg := loadConfig()

	store, err := NewStore(cfg.StoreDir, cfg.BackupEnabled)
	if err != nil {
		log.Fatalf("Failed to initialize store: %v", err)
	}
	if cfg.BackupEnabled {
		log.Printf("Revision backups enabled (bk/ directory)")
	}

	index := NewIndex()
	count, dur, err := index.Rebuild(store)
	if err != nil {
		log.Fatalf("Failed to rebuild index: %v", err)
	}
	log.Printf("Index rebuilt: %d objects in %v", count, dur)

	limiter := NewRateLimiter(cfg.RateLimitPerMin, cfg.RateLimitPerDay)
	defer limiter.Stop()

	var handler http.Handler
	var proxyCleanup []func() // cleanup functions for proxy mode

	// Auth widget config
	awCfg := AuthWidgetConfig{
		AuthHost:       cfg.AuthWidgetHost,
		AllowedOrigins: cfg.AuthWidgetAllowedOrigins,
	}
	if awCfg.AuthHost != "" {
		log.Printf("Auth widget enabled on %s", awCfg.AuthHost)
	}

	switch cfg.Mode {
	case "root":
		log.Printf("Starting dataverse hub (root mode) on %s (store: %s)", cfg.Addr, cfg.StoreDir)
		hub := NewHub(store, index, limiter, cfg.DefaultViewerRef)
		handler = hub.RouterWithAuthWidget(awCfg)

	default: // "proxy" is the default
		log.Printf("Starting dataverse hub (proxy mode) on %s -> %s (store: %s)", cfg.Addr, cfg.UpstreamURL, cfg.StoreDir)
		upstream := NewUpstream(cfg.UpstreamURL)

		// Probe upstream before serving
		if err := upstream.HealthCheck(); err != nil {
			log.Printf("WARN: upstream %s unreachable at startup: %v", cfg.UpstreamURL, err)
		} else {
			log.Printf("Upstream %s is reachable", cfg.UpstreamURL)
		}

		upstream.StartHealthChecker(30 * time.Second)
		proxyCleanup = append(proxyCleanup, upstream.Stop)

		pendingDir := filepath.Join(cfg.StoreDir, "sync_pending")
		pending := NewSyncPending(pendingDir, upstream, store, index)
		pending.Start()
		proxyCleanup = append(proxyCleanup, pending.Stop)

		proxy := NewProxy(store, index, limiter, cfg.DefaultViewerRef, upstream, pending)
		handler = proxy.RouterWithAuthWidget(awCfg)
	}

	srv := &http.Server{
		Addr:         cfg.Addr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	done := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("Received %v, shutting down...", sig)

		for _, fn := range proxyCleanup {
			fn()
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("Shutdown error: %v", err)
		}
		close(done)
	}()

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
	<-done
	log.Println("Server stopped")
}

func loadConfig() Config {
	cfg := Config{
		Mode:             envOr("DATAVERSE_MODE", "proxy"),
		UpstreamURL:      envOr("DATAVERSE_UPSTREAM_URL", "https://dataverse001.net"),
		Addr:             envOr("HUB_ADDR", ":5678"),
		StoreDir:         envOr("HUB_STORE_DIR", "./dataverse001"),
		RateLimitPerMin:  envOrInt("HUB_RATE_LIMIT_PER_MIN", 120),
		RateLimitPerDay:  envOrInt("HUB_RATE_LIMIT_PER_DAY", 20000),
		DefaultViewerRef: envOr("HUB_DEFAULT_VIEWER_REF", "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.b3f5a7c9-2d4e-4f60-9b8a-0c1d2e3f4a5b"),
		BackupEnabled:    envOr("HUB_BACKUP_ENABLED", "true") == "true",
		AuthWidgetHost:   envOr("HUB_AUTH_WIDGET_HOST", ""),
	}
	// Parse comma-separated allowed origins
	if origins := envOr("HUB_AUTH_WIDGET_ALLOWED_ORIGINS", ""); origins != "" {
		for _, o := range splitTrim(origins, ",") {
			if o != "" {
				cfg.AuthWidgetAllowedOrigins = append(cfg.AuthWidgetAllowedOrigins, o)
			}
		}
	}
	return cfg
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOrInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("WARN: invalid %s=%q, using default %d", key, v, def)
		return def
	}
	return n
}
