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

	"github.com/dataverse/hub/auth"
	"github.com/dataverse/hub/serving"
	"github.com/dataverse/hub/storage"
	"github.com/dataverse/hub/upstream"
	"github.com/dataverse/hub/vhost"
)

func main() {
	cfg := loadConfig()

	store, err := storage.NewStore(cfg.StoreDir, cfg.BackupEnabled)
	if err != nil {
		log.Fatalf("Failed to initialize store: %v", err)
	}
	if cfg.BackupEnabled {
		log.Printf("Revision backups enabled (bk/ directory)")
	}

	index := storage.NewIndex()
	count, dur, err := index.Rebuild(store)
	if err != nil {
		log.Fatalf("Failed to rebuild index: %v", err)
	}
	log.Printf("Index rebuilt: %d objects in %v", count, dur)

	// Build vhost resolver if BaseDomain is configured
	var resolver *vhost.Resolver
	if cfg.BaseDomain != "" {
		resolver = vhost.NewResolver(cfg.BaseDomain, cfg.TxtCacheTTL, nil)

		// Build hash map from indexed PAGE objects
		pageRefs := index.GetPageRefs()
		pageHashes := make(map[string]string, len(pageRefs))
		for _, ref := range pageRefs {
			pageHashes[vhost.PageHash(ref)] = ref
		}
		resolver.UpdateHashMap(pageHashes)
		log.Printf("Vhost enabled: base_domain=%s, %d PAGEs mapped", cfg.BaseDomain, len(pageHashes))
	}

	limiter := auth.NewRateLimiter(cfg.RateLimitPerMin, cfg.RateLimitPerDay)
	defer limiter.Stop()

	authStore := auth.NewAuthStore(cfg.AuthTokenExpiry)
	defer authStore.Stop()
	log.Printf("Auth enabled (token expiry: %v)", cfg.AuthTokenExpiry)

	var handler http.Handler
	var proxyCleanup []func() // cleanup functions for proxy mode

	// Legacy auth widget config (used when BaseDomain is empty)
	awCfg := auth.WidgetConfig{
		AuthHost:       cfg.AuthWidgetHost,
		AllowedOrigins: cfg.AuthWidgetAllowedOrigins,
	}

	// Deprecation warnings
	if resolver != nil && awCfg.AuthHost != "" {
		log.Printf("WARN: auth_widget_host is deprecated (vhosting handles this). Remove from config.")
	}
	if resolver != nil && len(cfg.AuthWidgetAllowedOrigins) > 0 {
		log.Printf("WARN: auth_widget_allowed_origins is deprecated (no CORS needed with vhosting). Remove from config.")
	}
	if resolver == nil && awCfg.AuthHost != "" {
		log.Printf("Auth widget enabled on %s (legacy mode)", awCfg.AuthHost)
	}

	switch cfg.Mode {
	case "root":
		log.Printf("Starting dataverse hub (root mode) on %s (store: %s)", cfg.Addr, cfg.StoreDir)
		hub := serving.NewHub(store, index, limiter, authStore, cfg.DefaultViewerRef)
		hub.Vhost = resolver
		handler = hub.RouterWithAuthWidget(awCfg)

	default: // "proxy" is the default
		log.Printf("Starting dataverse hub (proxy mode) on %s -> %s (store: %s)", cfg.Addr, cfg.UpstreamURL, cfg.StoreDir)
		up := upstream.NewClient(cfg.UpstreamURL)

		// Probe upstream before serving
		if err := up.HealthCheck(); err != nil {
			log.Printf("WARN: upstream %s unreachable at startup: %v", cfg.UpstreamURL, err)
		} else {
			log.Printf("Upstream %s is reachable", cfg.UpstreamURL)
		}

		up.StartHealthChecker(30 * time.Second)
		proxyCleanup = append(proxyCleanup, up.Stop)

		pendingDir := filepath.Join(cfg.StoreDir, "sync_pending")
		pending := upstream.NewSyncPending(pendingDir, up, store, index)
		pending.Start()
		proxyCleanup = append(proxyCleanup, pending.Stop)

		proxy := serving.NewProxy(store, index, limiter, authStore, cfg.DefaultViewerRef, up, pending)
		proxy.Vhost = resolver
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

