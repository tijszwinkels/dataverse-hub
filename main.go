package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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

	auth := NewAuthStore(cfg.AuthTokenExpiry)
	defer auth.Stop()
	log.Printf("Auth enabled (token expiry: %v)", cfg.AuthTokenExpiry)

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
		hub := NewHub(store, index, limiter, auth, cfg.DefaultViewerRef)
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

