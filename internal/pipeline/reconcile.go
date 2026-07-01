// Package pipeline wires discovery, parsing, rendering and applying together
// behind a debounced trigger, so a burst of Docker events (e.g. `compose up`)
// collapses into a single config regeneration.
package pipeline

import (
	"context"
	"log"
	"sort"
	"time"

	"github.com/ekho/gobackup-docker/internal/apply"
	"github.com/ekho/gobackup-docker/internal/docker"
	"github.com/ekho/gobackup-docker/internal/labels"
	"github.com/ekho/gobackup-docker/internal/render"
	"gopkg.in/yaml.v3"
)

// Config holds the reconciler's runtime settings.
type Config struct {
	DefaultsPath     string        // path to defaults.yml (profiles)
	Instance         string        // GOBACKUP_DOCKER_INSTANCE ("" = manage all)
	HostID           string        // docker host id, suffix for auto model names
	ExposedByDefault bool          // include containers without gobackup.enable
	Debounce         time.Duration // collapse event bursts
}

// Reconciler regenerates the gobackup config on demand.
type Reconciler struct {
	cfg    Config
	docker *docker.Client
	writer *apply.FileWriter
}

func NewReconciler(cfg Config, dc *docker.Client, w *apply.FileWriter) *Reconciler {
	return &Reconciler{cfg: cfg, docker: dc, writer: w}
}

// Run drives the debounced loop until ctx is cancelled. fire() (passed to the
// docker/defaults watchers) requests a reconcile; bursts are coalesced by both
// the size-1 trigger channel and the debounce timer.
func (r *Reconciler) Run(ctx context.Context, trigger <-chan struct{}) {
	timer := time.NewTimer(r.cfg.Debounce)
	if !timer.Stop() {
		<-timer.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger:
			timer.Reset(r.cfg.Debounce)
		case <-timer.C:
			if err := r.reconcile(ctx); err != nil {
				log.Printf("[reconcile] failed (keeping last config): %v", err)
			}
		}
	}
}

// reconcile does one full pass: list → parse+gate → render → apply. Any error
// returns without applying, so the previously written config stays in place.
func (r *Reconciler) reconcile(ctx context.Context) error {
	containers, err := r.docker.List(ctx)
	if err != nil {
		return err
	}

	sources := make([]render.Source, 0, len(containers))
	for _, c := range containers {
		p := labels.Parse(c.Labels, r.cfg.ExposedByDefault)
		if !p.Enabled || len(p.Model) == 0 {
			continue
		}
		// Scope: skip containers explicitly claimed by a different instance.
		if r.cfg.Instance != "" && p.Instance != "" && p.Instance != r.cfg.Instance {
			continue
		}
		sources = append(sources, render.Source{
			Container: c.Name,
			Name:      p.Name,
			Profile:   p.Profile,
			Model:     p.Model,
		})
	}

	// Reload profiles every pass so defaults.yml edits take effect; a parse
	// error here aborts the reconcile and keeps the last-good file.
	profiles, err := render.LoadProfiles(r.cfg.DefaultsPath)
	if err != nil {
		return err
	}

	cfg := render.Build(sources, profiles, r.cfg.HostID, r.cfg.Instance)
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	changed, err := r.writer.Apply(out)
	if err != nil {
		return err
	}
	models, _ := cfg["models"].(map[string]any)
	if changed {
		log.Printf("[reconcile] wrote %s: %d model(s) from %d container(s)", r.writer.Path, len(models), len(sources))
		// File archives tar the gobackup container's own filesystem, not the
		// labeled container's — remind the operator to mount the source paths.
		if a := modelsWithArchive(models); len(a) > 0 {
			log.Printf("[reconcile] note: models %v use file archive — ensure their include paths are mounted into the gobackup container", a)
		}
	}
	return nil
}

func modelsWithArchive(models map[string]any) []string {
	var names []string
	for name, m := range models {
		if mm, ok := m.(map[string]any); ok {
			if _, has := mm["archive"]; has {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}
