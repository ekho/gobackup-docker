package render

import (
	"errors"
	"fmt"
	"log"
)

// TemplateData is exposed to {{ ... }} tokens in label/profile values.
type TemplateData struct {
	Model     string // final model name (see ModelName)
	Container string // container name
	Host      string // docker host id (daemon hostname)
	Instance  string // GOBACKUP_DOCKER_INSTANCE ("" if unset)
}

// Source is one container's contribution: its single model body (from labels),
// an optional explicit name, and which profile it inherits.
type Source struct {
	Container string
	Name      string // gobackup.name ("" => auto)
	Profile   string // "" => "default"
	Model     map[string]any
}

// ModelName computes the gobackup model name for a container's backup model.
// This name is BOTH the key under `models:` AND the value of {{ .Model }}, so it
// also shapes backup paths like /backups/{{ .Model }} and gobackup's retention
// state key — it must be stable.
//
//   - explicit gobackup.name  → used verbatim (e.g. "mynextcloud").
//   - otherwise               → "<container>-<host>", where host is the docker
//     daemon hostname, so backups from different hosts don't collide.
//   - if host is empty (unknown / disabled) → just the container name.
func ModelName(container, customName, hostID string) string {
	if customName != "" {
		return customName
	}
	if hostID != "" {
		return container + "-" + hostID
	}
	return container
}

// Build assembles the full gobackup config document: for each container it
// deep-merges the labels over the inherited profile, applies "!none" opt-outs,
// expands templates, validates, and keys the result by ModelName. Invalid or
// colliding models are skipped with a log (fail-closed) rather than emitted.
func Build(sources []Source, profiles Profiles, hostID, instance string) map[string]any {
	models := map[string]any{}

	for _, src := range sources {
		base, ok := resolveProfile(profiles, src)
		if !ok {
			continue // unknown explicit profile: already logged, skip container
		}
		name := ModelName(src.Container, src.Name, hostID)

		merged := deepCopy(base).(map[string]any)
		deepMerge(merged, src.Model)
		pruneOptOut(merged)

		expanded, err := expandTemplates(merged, TemplateData{
			Model: name, Container: src.Container, Host: hostID, Instance: instance,
		})
		if err != nil {
			log.Printf("[render] model %q: template error: %v; skipping", name, err)
			continue
		}
		m := expanded.(map[string]any)

		if err := validateModel(m); err != nil {
			log.Printf("[render] model %q invalid: %v; skipping", name, err)
			continue
		}
		if _, dup := models[name]; dup {
			log.Printf("[render] model name collision %q (container %q); skipping duplicate", name, src.Container)
			continue
		}
		models[name] = m
	}
	return map[string]any{"models": models}
}

func resolveProfile(profiles Profiles, src Source) (map[string]any, bool) {
	name := src.Profile
	if name == "" {
		name = "default"
	}
	if base, ok := profiles[name]; ok {
		return base, true
	}
	if src.Profile == "" {
		return map[string]any{}, true // no explicit profile and no "default": label-only mode
	}
	log.Printf("[render] container %q: unknown profile %q; skipping", src.Container, src.Profile)
	return nil, false
}

// validateModel enforces gobackup's own rule (≥1 storage AND databases-or-archive)
// on OUR side, so we never write a model gobackup would reject and zero out.
func validateModel(m map[string]any) error {
	if st, _ := m["storages"].(map[string]any); len(st) == 0 {
		return errors.New("no storages")
	}
	db, _ := m["databases"].(map[string]any)
	if _, hasArchive := m["archive"]; len(db) == 0 && !hasArchive {
		return fmt.Errorf("needs at least one of databases or archive")
	}
	return nil
}
