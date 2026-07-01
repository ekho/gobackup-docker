// Command gobackup-docker is a Traefik-style supervisor: it watches Docker
// containers, reads gobackup.* labels, and renders a gobackup.yml that the
// stock gobackup container hot-reloads. See docs/ARCHITECTURE.md.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/ekho/gobackup-docker/internal/apply"
	"github.com/ekho/gobackup-docker/internal/docker"
	"github.com/ekho/gobackup-docker/internal/pipeline"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := pipeline.Config{
		DefaultsPath:     env("GOBACKUP_DOCKER_DEFAULTS", "/etc/gobackup-docker/defaults.yml"),
		Instance:         os.Getenv("GOBACKUP_DOCKER_INSTANCE"),
		ExposedByDefault: envBool("GOBACKUP_DOCKER_EXPOSED_BY_DEFAULT", false),
		Debounce:         envDur("GOBACKUP_DOCKER_DEBOUNCE", 500*time.Millisecond),
	}
	outputPath := env("GOBACKUP_DOCKER_OUTPUT", "/etc/gobackup/gobackup.yml")

	dc, err := docker.New(ctx)
	if err != nil {
		log.Fatalf("[main] docker: %v", err)
	}
	defer dc.Close()

	// Host id (suffix for auto model names): env override, else daemon hostname.
	cfg.HostID = os.Getenv("GOBACKUP_DOCKER_HOST_ID")
	if cfg.HostID == "" {
		if id, err := dc.HostID(ctx); err != nil {
			log.Printf("[main] cannot read docker host id (%v); auto model names will omit the host suffix", err)
		} else {
			cfg.HostID = id
		}
	}

	writer := &apply.FileWriter{Path: outputPath}
	rec := pipeline.NewReconciler(cfg, dc, writer)

	trigger := make(chan struct{}, 1)
	fire := func() {
		select {
		case trigger <- struct{}{}:
		default: // a reconcile is already pending
		}
	}

	go dc.WatchEvents(ctx, fire)
	go pipeline.WatchFile(ctx, cfg.DefaultsPath, fire)
	fire() // initial reconcile

	log.Printf("[main] gobackup-docker up: output=%s defaults=%s host=%q instance=%q exposedByDefault=%v debounce=%s",
		outputPath, cfg.DefaultsPath, cfg.HostID, cfg.Instance, cfg.ExposedByDefault, cfg.Debounce)

	rec.Run(ctx, trigger)
	log.Printf("[main] shutdown")
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		log.Printf("[main] bad %s=%q, using %v", key, v, def)
		return def
	}
	return b
}

func envDur(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("[main] bad %s=%q, using %s", key, v, def)
		return def
	}
	return d
}
