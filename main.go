package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	cfg := loadConfig()

	log.Printf("Starting dataverse hub on %s (store: %s)", cfg.Addr, cfg.StoreDir)

	store, err := NewStore(cfg.StoreDir)
	if err != nil {
		log.Fatalf("Failed to initialize store: %v", err)
	}

	index := NewIndex()
	count, dur, err := index.Rebuild(store)
	if err != nil {
		log.Fatalf("Failed to rebuild index: %v", err)
	}
	log.Printf("Index rebuilt: %d objects in %v", count, dur)

	limiter := NewRateLimiter(cfg.RateLimitPerMin, cfg.RateLimitPerDay)
	defer limiter.Stop()

	hub := NewHub(store, index, limiter)
	srv := &http.Server{
		Addr:         cfg.Addr,
		Handler:      hub.Router(),
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
	return Config{
		Addr:            envOr("HUB_ADDR", ":8080"),
		StoreDir:        envOr("HUB_STORE_DIR", "./dataverse001"),
		RateLimitPerMin: envOrInt("HUB_RATE_LIMIT_PER_MIN", 60),
		RateLimitPerDay: envOrInt("HUB_RATE_LIMIT_PER_DAY", 10000),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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
