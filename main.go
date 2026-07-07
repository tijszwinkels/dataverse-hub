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

	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/events"
	"github.com/tijszwinkels/dataverse-hub/realm"
	"github.com/tijszwinkels/dataverse-hub/serving"
	"github.com/tijszwinkels/dataverse-hub/storage"
	"github.com/tijszwinkels/dataverse-hub/upstream"
	"github.com/tijszwinkels/dataverse-hub/vhost"
)

func main() {
	cfg, configPath := loadConfig()

	store, err := storage.NewStore(cfg.StoreDir, cfg.BackupEnabled)
	if err != nil {
		log.Fatalf("Failed to initialize store: %v", err)
	}
	if cfg.BackupEnabled {
		log.Printf("Revision backups enabled (bk/ directory)")
	}

	// Shared realms
	shared := realm.NewSharedRealms()
	if configPath != "" {
		realms, err := loadRealmsFromFile(configPath)
		if err != nil {
			log.Fatalf("Failed to load shared realms: %v", err)
		}
		if realms != nil {
			shared.Load(realms)
			log.Printf("Shared realms: %d realms configured", shared.Count())
		}
	}

	index := storage.NewIndex(shared)
	count, dur, err := index.Rebuild(store)
	if err != nil {
		log.Fatalf("Failed to rebuild index: %v", err)
	}
	log.Printf("Index rebuilt: %d objects in %v", count, dur)

	// Build vhost resolver if BaseDomain is configured
	var resolver *vhost.Resolver
	if cfg.BaseDomain != "" && cfg.VhostMode != serving.VhostModeOff {
		resolver = vhost.NewResolver(cfg.BaseDomain, cfg.TxtCacheTTL, nil)

		// Build hash map from indexed PAGE objects
		pageRefs := index.GetPageRefs()
		pageHashes := make(map[string]string, len(pageRefs))
		for _, ref := range pageRefs {
			pageHashes[vhost.PageHash(ref)] = ref
		}
		resolver.UpdateHashMap(pageHashes)
		log.Printf("Vhost enabled: mode=%s, base_domain=%s, %d PAGEs mapped", cfg.VhostMode, cfg.BaseDomain, len(pageHashes))
	} else if cfg.VhostMode != serving.VhostModeOff {
		log.Printf("Vhost disabled: base_domain is empty (vhost_mode=%s)", cfg.VhostMode)
	} else {
		log.Printf("Vhost disabled: vhost_mode=off")
	}

	limiter := auth.NewRateLimiter(cfg.RateLimitPerMin, cfg.RateLimitPerDay)
	defer limiter.Stop()

	authStore := auth.NewAuthStore(cfg.AuthTokenExpiry)
	defer authStore.Stop()
	log.Printf("Auth enabled (token expiry: %v)", cfg.AuthTokenExpiry)

	// Events journal (change feed). Disabled → nil log → /events serves 404.
	var elog *events.Log
	if cfg.EventsEnabled {
		var err error
		elog, err = events.Open(filepath.Join(cfg.StoreDir, "events"), events.Options{Retention: cfg.EventsRetention})
		if err != nil {
			log.Fatalf("Failed to open events journal: %v", err)
		}
		log.Printf("Events enabled (retention: %v, max subscribers: %d)", cfg.EventsRetention, cfg.EventsMaxSubscribers)
	} else {
		log.Printf("Events disabled (events_enabled = false)")
	}

	var handler http.Handler
	var cleanup []func() // shutdown hooks, run in order before srv.Shutdown

	switch cfg.Mode {
	case "root":
		log.Printf("Starting dataverse hub (root mode) on %s (store: %s)", cfg.Addr, cfg.StoreDir)
		hub := serving.NewHub(store, index, limiter, authStore, cfg.DefaultViewerRef, shared)
		hub.Vhost = resolver
		hub.VhostMode = cfg.VhostMode
		hub.Events = elog
		hub.EventsMaxSubscribers = cfg.EventsMaxSubscribers
		handler = hub.Router()

	default: // "proxy" is the default
		log.Printf("Starting dataverse hub (proxy mode) on %s -> %s (store: %s, upstream_push: %s)", cfg.Addr, cfg.UpstreamURL, cfg.StoreDir, cfg.UpstreamPush)
		up := upstream.NewClient(cfg.UpstreamURL)

		// Probe upstream before serving
		if err := up.HealthCheck(); err != nil {
			log.Printf("WARN: upstream %s unreachable at startup: %v", cfg.UpstreamURL, err)
		} else {
			log.Printf("Upstream %s is reachable", cfg.UpstreamURL)
		}

		up.StartHealthChecker(30 * time.Second)
		cleanup = append(cleanup, up.Stop)

		pendingDir := filepath.Join(cfg.StoreDir, "sync_pending")
		pending := upstream.NewSyncPending(pendingDir, up, store, index)
		pending.Events = elog
		pending.Start()
		cleanup = append(cleanup, pending.Stop)

		proxy := serving.NewProxy(store, index, limiter, authStore, cfg.DefaultViewerRef, up, pending, shared)
		proxy.UpstreamPush = cfg.UpstreamPush
		proxy.Vhost = resolver
		proxy.VhostMode = cfg.VhostMode
		proxy.Events = elog
		proxy.EventsMaxSubscribers = cfg.EventsMaxSubscribers
		proxy.EventsPrefetch = cfg.EventsPrefetch
		handler = proxy.Router()

		// Subscribe to the upstream change feed: apply/pass-through events
		// into our own journal, revalidate the cache on upstream resets.
		if elog != nil && cfg.EventsUpstream {
			sub := upstream.NewSubscriber(
				cfg.UpstreamURL,
				filepath.Join(cfg.StoreDir, "events", "upstream-cursor.json"),
				up,
				upstream.SubscriberCallbacks{
					OnEvent: proxy.ApplyUpstreamEvent,
					OnReset: proxy.RevalidateAgainstUpstream,
				},
			)
			sub.Start()
			cleanup = append(cleanup, sub.Stop)
			log.Printf("Events: subscribed to upstream %s/events (prefetch: %v)", cfg.UpstreamURL, cfg.EventsPrefetch)
		}
	}

	// Close the journal before srv.Shutdown: closing ends all subscriptions,
	// which makes long-lived SSE handlers return, which lets Shutdown drain.
	if elog != nil {
		cleanup = append(cleanup, func() { elog.Close() })
	}

	srv := &http.Server{
		Addr:         cfg.Addr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// SIGHUP: reload shared realm config
	go func() {
		sigHUP := make(chan os.Signal, 1)
		signal.Notify(sigHUP, syscall.SIGHUP)
		for range sigHUP {
			log.Println("SIGHUP received, reloading shared realms...")
			if configPath == "" {
				log.Println("WARN: no config file provided, nothing to reload")
				continue
			}
			realms, err := loadRealmsFromFile(configPath)
			if err != nil {
				log.Printf("ERROR: realm reload failed, keeping previous config: %v", err)
				continue
			}
			if realms == nil {
				log.Println("No [realms] section in config, keeping previous config")
				continue
			}
			shared.Load(realms)
			log.Printf("Reloaded shared realms: %d realms configured", shared.Count())
		}
	}()

	// Graceful shutdown
	done := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("Received %v, shutting down...", sig)

		for _, fn := range cleanup {
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
