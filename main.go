package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func sampleInterval() time.Duration {
	d, err := time.ParseDuration(env("OPENRDS_METRICS_INTERVAL", defaultSampleInterval.String()))
	if err != nil || d < 5*time.Second || d > 5*time.Minute {
		log.Printf("OPENRDS_METRICS_INTERVAL must be a duration from 5s to 5m; using %s", defaultSampleInterval)
		return defaultSampleInterval
	}
	return d
}

func main() {
	if len(os.Args) > 1 {
		os.Exit(command(os.Args[1:], os.Stdout, os.Stderr))
	}
	dir := env("OPENRDS_DATA_DIR", ".openrds")
	m, err := openManager(dir, "engines", dockerCLI{})
	if err != nil {
		log.Fatal(err)
	}
	token, err := adminToken(dir)
	if err != nil {
		log.Fatal(err)
	}
	people, err := openAccounts(filepath.Join(dir, "users.json"))
	if err != nil {
		log.Fatal(err)
	}
	m.history.interval = sampleInterval()
	if m.engine, err = dockerEngine(); err != nil {
		m.history.unavailable = "Metric history is off: " + err.Error() + "."
		log.Print(m.history.unavailable)
	}
	m.history.load(time.Now())
	prepare, cancelPrepare := context.WithTimeout(context.Background(), 2*time.Minute)
	m.prepareDisks(prepare)
	cancelPrepare()
	srv := &http.Server{Addr: env("OPENRDS_LISTEN", "127.0.0.1:8080"), Handler: handler(m, auth{token: token, accounts: people}),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: time.Minute}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	m.offsite.ctx = ctx
	go m.recoverDisks(ctx) // after prepareDisks attached the disk images
	go m.maintain(ctx)
	go m.sampleMetrics(ctx)
	go m.syncAllImages(ctx) // an updated console brings new helper images
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	log.Printf("OpenRDS listening on %s", srv.Addr)
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}
	if err := m.history.save(); err != nil {
		log.Printf("saving metric history: %v", err)
	}
}
