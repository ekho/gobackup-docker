// Command gobackup-docker is a label-driven supervisor: it watches Docker
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
	"github.com/ekho/gobackup-docker/internal/container"
	"github.com/ekho/gobackup-docker/internal/docker"
	"github.com/ekho/gobackup-docker/internal/pipeline"
	"github.com/ekho/gobackup-docker/internal/webapi"
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
	rec := pipeline.NewReconciler(cfg, dc, writer).
		WithContainerManager(dc).
		WithGobackupSpec(readSelfContainerConfig(ctx, dc))

	trigger := make(chan struct{}, 1)
	fire := func() {
		select {
		case trigger <- struct{}{}:
		default: // a reconcile is already pending
		}
	}

	// Optional control-plane API (supervisor state + proxied "backup now").
	if addr := os.Getenv("GOBACKUP_DOCKER_HTTP_ADDR"); addr != "" {
		api := &webapi.Server{Status: rec.Status, GobackupURL: os.Getenv("GOBACKUP_DOCKER_GOBACKUP_URL")}
		go func() {
			if err := api.Serve(ctx, addr); err != nil {
				log.Printf("[api] server error: %v", err)
			}
		}()
	}

	go dc.WatchEvents(ctx, fire)
	go pipeline.WatchFile(ctx, cfg.DefaultsPath, fire)
	fire() // initial reconcile

	log.Printf("[main] gobackup-docker up: output=%s defaults=%s host=%q instance=%q exposedByDefault=%v debounce=%s",
		outputPath, cfg.DefaultsPath, cfg.HostID, cfg.Instance, cfg.ExposedByDefault, cfg.Debounce)

	rec.Run(ctx, trigger)
	log.Printf("[main] shutdown")
}

// readSelfContainerConfig inspects the supervisor's own container (found via its
// hostname, which Docker sets to the container id by default) and parses its
// gobackup_container.* labels into a container.Config used when recreating the
// managed gobackup container. Failures degrade gracefully to an empty Config
// (the recreate then falls back to the existing container's settings).
func readSelfContainerConfig(ctx context.Context, dc *docker.Client) container.Config {
	if v := os.Getenv("GOBACKUP_DOCKER_SELF_ID"); v != "" {
		if self, err := dc.ContainerInspect(ctx, v); err == nil {
			return container.Parse(self.Labels)
		} else {
			log.Printf("[main] self-inspect via GOBACKUP_DOCKER_SELF_ID=%s failed: %v", v, err)
			return container.Config{}
		}
	}
	host, err := os.Hostname()
	if err != nil {
		log.Printf("[main] cannot read hostname for self-inspect; gobackup_container.* labels ignored: %v", err)
		return container.Config{}
	}
	self, err := dc.ContainerInspect(ctx, host)
	if err != nil {
		log.Printf("[main] self-inspect (%s) failed; gobackup_container.* labels ignored: %v", host, err)
		return container.Config{}
	}
	return container.Parse(self.Labels)
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
