// Package pipeline wires discovery, parsing, rendering and applying together
// behind a debounced trigger, so a burst of Docker events (e.g. `compose up`)
// collapses into a single config regeneration.
package pipeline

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/mount"

	"github.com/ekho/gobackup-docker/internal/apply"
	"github.com/ekho/gobackup-docker/internal/container"
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

// Reconciler regenerates the gobackup config on demand. When a ContainerManager
// is provided via WithContainerManager, it also discovers archive volumes from
// source containers and ensures the gobackup container has the right mounts.
type Reconciler struct {
	cfg          Config
	lister       Lister
	writer       *apply.FileWriter
	containerMgr ContainerManager // optional, for archive volume support
	gobackupSpec container.Config // gobackup_container.* from the supervisor's own labels

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

// WithContainerManager attaches a ContainerManager for archive volume support.
func (r *Reconciler) WithContainerManager(mgr ContainerManager) *Reconciler {
	r.containerMgr = mgr
	return r
}

// WithGobackupSpec supplies the gobackup_container.* config parsed from the
// supervisor's own labels; it shapes the recreated gobackup container's spec.
func (r *Reconciler) WithGobackupSpec(cfg container.Config) *Reconciler {
	r.gobackupSpec = cfg
	return r
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
	models, nSources, changed, modelToContainer, creds, err := r.render(ctx)
	if err != nil {
		r.mu.Lock()
		r.status.LastError = err.Error()
		r.mu.Unlock()
		return err
	}

	// Phase 2: volume discovery — inspect source containers and re-mount their
	// volumes into the gobackup engine under /volumes/<model>/..., transforming
	// both archive.includes (read-only) and sqlite databases' `path` (read-write).
	var managedMounts []docker.MountDef
	if r.containerMgr != nil {
		archiveMounts, aerr := r.processArchiveVolumes(ctx, models, modelToContainer)
		if aerr != nil {
			log.Printf("[reconcile] archive volume discovery failed: %v (continuing with label-only config)", aerr)
		}
		sqliteMounts := r.processSqliteVolumes(ctx, models, modelToContainer)
		managedMounts = dedupMountsByTarget(append(archiveMounts, sqliteMounts...))
		if len(managedMounts) > 0 {
			// Re-marshal and write since models' paths were transformed.
			out, marshalErr := yaml.Marshal(map[string]any{"models": models})
			if marshalErr != nil {
				log.Printf("[reconcile] re-marshal after volume mapping: %v", marshalErr)
			} else if changed2, applyErr := r.writer.Apply(out); applyErr != nil {
				log.Printf("[reconcile] write after volume mapping: %v", applyErr)
			} else {
				changed = changed || changed2
			}
		}
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
	}

	// Phase 3: resolve credential sources (from the client containers) into engine
	// additions — env vars (_env) and secret bind-mounts + command exports (_file).
	extras := engineExtras{ExtraMounts: managedMounts}
	if r.containerMgr != nil && len(creds) > 0 {
		ec := resolveCreds(ctx, r.containerMgr, creds)
		extras.ExtraMounts = append(extras.ExtraMounts, ec.secretMounts...)
		extras.EnvVars = ec.envVars
		extras.SecretExports = ec.secretExports
	}

	// Phase 4: ensure the gobackup container matches the desired spec (mounts,
	// credential env, secret command-wrapper). Only touch it when there is
	// something to manage — archive volumes or credentials.
	if r.containerMgr != nil && (len(extras.ExtraMounts) > 0 || len(extras.EnvVars) > 0 || len(extras.SecretExports) > 0) {
		if _, recreated, err := ensureGobackupContainer(ctx, r.containerMgr, r.gobackupSpec, extras); err != nil {
			log.Printf("[reconcile] gobackup container sync: %v", err)
		} else if recreated {
			log.Printf("[reconcile] gobackup container recreated (mounts/credentials)")
		}
	}

	return nil
}

// resolvedEngineCreds are credential sources resolved against the client
// containers, ready to feed the engine recreate.
type resolvedEngineCreds struct {
	envVars       []string
	secretMounts  []docker.MountDef
	secretExports []secretExport
}

// resolveCreds reads each credential's value source from its client container:
// _env → the value of an env var (materialized into the engine's env); _file →
// the host source of the secret's bind mount (re-mounted into the engine under
// /gobackup-secrets/<VAR> and cat'd at start via the command wrapper). Sources
// that can't be resolved (missing env var, or a secret with no host bind source —
// swarm/environment secrets) are skipped with a log; the credential renders as an
// empty ${VAR}, which gobackup treats as a connection failure (fail-closed).
func resolveCreds(ctx context.Context, mgr ContainerManager, creds []render.ResolvedCred) resolvedEngineCreds {
	var out resolvedEngineCreds
	inspects := map[string]docker.InspectResult{}
	seenTarget := map[string]bool{}

	for _, c := range creds {
		info, ok := inspects[c.ContainerID]
		if !ok {
			var err error
			info, err = mgr.ContainerInspect(ctx, c.ContainerID)
			if err != nil {
				log.Printf("[creds] inspect source %s for %s: %v; skipping", shortID(c.ContainerID), c.Var, err)
				continue
			}
			inspects[c.ContainerID] = info
		}

		switch c.Kind {
		case labels.CredEnv:
			val, found := lookupEnv(info.Env, c.Ref)
			if !found {
				log.Printf("[creds] env var %q not set in source %s; skipping %s", c.Ref, shortID(c.ContainerID), c.Var)
				continue
			}
			out.envVars = append(out.envVars, c.Var+"="+val)

		case labels.CredFile:
			mp, _, err := findMountForPath(c.Ref, buildDestMap(info.Mounts))
			if err != nil {
				log.Printf("[creds] secret %q is not a bind-mounted file in source %s (swarm/environment secrets unsupported); skipping %s", c.Ref, shortID(c.ContainerID), c.Var)
				continue
			}
			src := mountSource(mp)
			if src == "" {
				log.Printf("[creds] secret %q in source %s has no host source (tmpfs); skipping %s", c.Ref, shortID(c.ContainerID), c.Var)
				continue
			}
			target := secretMountPrefix + c.Var
			if seenTarget[target] {
				continue
			}
			seenTarget[target] = true
			out.secretMounts = append(out.secretMounts, docker.MountDef{
				Type: mount.TypeBind, Source: src, Target: target, ReadOnly: true,
			})
			out.secretExports = append(out.secretExports, secretExport{Var: c.Var, Path: target})
		}
	}
	return out
}

func lookupEnv(env []string, name string) (string, bool) {
	prefix := name + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return e[len(prefix):], true
		}
	}
	return "", false
}

// render performs list→parse→build→apply, returning the models map, the
// number of contributing containers, whether the file changed, and a mapping
// from model name → source container ID for archive volume discovery.
func (r *Reconciler) render(ctx context.Context) (models map[string]any, nSources int, changed bool, modelToContainer map[string]string, creds []render.ResolvedCred, err error) {
	containers, err := r.lister.List(ctx)
	if err != nil {
		return nil, 0, false, nil, nil, err
	}

	sources := make([]render.Source, 0, len(containers))
	modelToContainer = make(map[string]string, len(containers))
	for _, c := range containers {
		p := labels.Parse(c.Labels, r.cfg.ExposedByDefault)
		if !p.Enabled || len(p.Model) == 0 {
			continue
		}
		// Scope: skip containers explicitly claimed by a different instance.
		if r.cfg.Instance != "" && p.Instance != "" && p.Instance != r.cfg.Instance {
			continue
		}
		modelName := render.ModelName(c.Name, p.Name, r.cfg.HostID)
		modelToContainer[modelName] = c.ID
		sources = append(sources, render.Source{
			Container:   c.Name,
			ContainerID: c.ID,
			Name:        p.Name,
			Profile:     p.Profile,
			Model:       p.Model,
			CredRefs:    p.CredRefs,
		})
	}

	// Reload profiles every pass so defaults.yml edits take effect; a parse
	// error here aborts the reconcile and keeps the last-good file.
	profiles, err := render.LoadProfiles(r.cfg.DefaultsPath)
	if err != nil {
		return nil, 0, false, nil, nil, err
	}

	cfg, creds := render.BuildWithCreds(sources, profiles, r.cfg.HostID, r.cfg.Instance)
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, 0, false, nil, nil, err
	}

	changed, err = r.writer.Apply(out)
	if err != nil {
		return nil, 0, false, nil, nil, err
	}
	models, _ = cfg["models"].(map[string]any)
	return models, len(sources), changed, modelToContainer, creds, nil
}

// processArchiveVolumes discovers volume mounts for models with archive.includes
// and updates the models map with transformed paths. Returns the combined mount
// list for the gobackup container.
func (r *Reconciler) processArchiveVolumes(
	ctx context.Context,
	models map[string]any,
	modelToContainer map[string]string,
) ([]docker.MountDef, error) {
	var vols []ArchiveVolumes
	for modelName, m := range models {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		archive, _ := mm["archive"].(map[string]any)
		if archive == nil {
			continue
		}
		includes, _ := toStrSlice(archive["includes"])
		excludes, _ := toStrSlice(archive["excludes"])
		if len(includes) == 0 && len(excludes) == 0 {
			continue
		}

		containerID := modelToContainer[modelName]
		if containerID == "" {
			log.Printf("[reconcile] model %q: no source container ID for archive discovery", modelName)
			continue
		}

		av, err := discoverArchiveVolumes(ctx, r.containerMgr, containerID, modelName, includes, excludes)
		if err != nil {
			log.Printf("[reconcile] model %q: archive volume discovery: %v (skipping archive transform)", modelName, err)
			continue
		}
		if len(av.Includes) > 0 || len(av.Excludes) > 0 {
			vols = append(vols, av)
		}
	}

	if len(vols) == 0 {
		return nil, nil
	}
	return applyArchiveVolumes(models, vols), nil
}

// processSqliteVolumes re-mounts the backing volume of each sqlite database's
// `path` into the gobackup engine and rewrites the path to the mounted location
// (analogous to archive, but read-write). Returns the mounts to add.
func (r *Reconciler) processSqliteVolumes(ctx context.Context, models map[string]any, modelToContainer map[string]string) []docker.MountDef {
	var mounts []docker.MountDef
	for modelName, m := range models {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		dbs, _ := mm["databases"].(map[string]any)
		if !hasSqlitePath(dbs) {
			continue
		}
		containerID := modelToContainer[modelName]
		if containerID == "" {
			log.Printf("[reconcile] model %q: no source container for sqlite mount", modelName)
			continue
		}
		info, err := r.containerMgr.ContainerInspect(ctx, containerID)
		if err != nil {
			log.Printf("[reconcile] model %q: inspect for sqlite mount: %v", modelName, err)
			continue
		}
		mounts = append(mounts, sqliteMountsForModel(buildDestMap(info.Mounts), modelName, dbs)...)
	}
	return mounts
}

// hasSqlitePath reports whether any database is a sqlite DB with an untransformed path.
func hasSqlitePath(dbs map[string]any) bool {
	for _, dbv := range dbs {
		db, ok := dbv.(map[string]any)
		if !ok || db["type"] != "sqlite" {
			continue
		}
		if p, _ := db["path"].(string); p != "" && !strings.HasPrefix(p, "/volumes/") {
			return true
		}
	}
	return false
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

// toStrSlice converts a yaml-decoded value to a string slice.
// YAML unmarshal produces []any for arrays; this handles both.
func toStrSlice(v any) ([]string, bool) {
	switch val := v.(type) {
	case []string:
		return val, true
	case []any:
		out := make([]string, 0, len(val))
		for _, item := range val {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out, len(out) > 0 || len(val) == 0
	default:
		return nil, false
	}
}
