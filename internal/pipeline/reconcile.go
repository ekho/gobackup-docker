// Package pipeline wires discovery, parsing, rendering and applying together
// behind a debounced trigger, so a burst of Docker events (e.g. `compose up`)
// collapses into a single config regeneration.
package pipeline

import (
	"context"
	"log"
	"sort"
	"sync"
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

// Status is a snapshot of the last reconcile, exposed via the control-plane API.
type Status struct {
	Instance         string   `json:"instance"`
	HostID           string   `json:"host_id"`
	ExposedByDefault bool     `json:"exposed_by_default"`
	Models           []string `json:"models"`
	Containers       int      `json:"containers"`
	LastRenderUnix   int64    `json:"last_render_unix"`
	LastChanged      bool     `json:"last_changed"`
	LastError        string   `json:"last_error,omitempty"`
}

// Lister enumerates candidate containers. Satisfied by *docker.Client; faked in tests.
type Lister interface {
	List(ctx context.Context) ([]docker.Container, error)
}

// Reconciler regenerates the gobackup config on demand.
type Reconciler struct {
	cfg    Config
	lister Lister
	writer *apply.FileWriter

	mu     sync.Mutex
	status Status
}

func NewReconciler(cfg Config, lister Lister, w *apply.FileWriter) *Reconciler {
	return &Reconciler{
		cfg:    cfg,
		lister: lister,
		writer: w,
		status: Status{Instance: cfg.Instance, HostID: cfg.HostID, ExposedByDefault: cfg.ExposedByDefault},
	}
}

// Status returns a copy of the latest reconcile snapshot (safe for concurrent use).
func (r *Reconciler) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.status
	s.Models = append([]string(nil), r.status.Models...)
	return s
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
	models, nSources, changed, err := r.render(ctx)
	if err != nil {
		r.mu.Lock()
		r.status.LastError = err.Error()
		r.mu.Unlock()
		return err
	}

	names := modelNames(models)
	r.mu.Lock()
	r.status.Models = names
	r.status.Containers = nSources
	r.status.LastRenderUnix = time.Now().Unix()
	r.status.LastChanged = changed
	r.status.LastError = ""
	r.mu.Unlock()

	if changed {
		log.Printf("[reconcile] wrote %s: %d model(s) from %d container(s)", r.writer.Path, len(names), nSources)
		if a := modelsWithArchive(models); len(a) > 0 {
			log.Printf("[reconcile] note: models %v use file archive — ensure their include paths are mounted into the gobackup container", a)
		}
	}
	return nil
}

// render performs the pure list→parse→build→apply, returning the models map, the
// number of contributing containers, and whether the file changed.
func (r *Reconciler) render(ctx context.Context) (models map[string]any, nSources int, changed bool, err error) {
	containers, err := r.lister.List(ctx)
	if err != nil {
		return nil, 0, false, err
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
		return nil, 0, false, err
	}

	cfg := render.Build(sources, profiles, r.cfg.HostID, r.cfg.Instance)
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, 0, false, err
	}

	changed, err = r.writer.Apply(out)
	if err != nil {
		return nil, 0, false, err
	}
	models, _ = cfg["models"].(map[string]any)
	return models, len(sources), changed, nil
}

func modelNames(models map[string]any) []string {
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
