package render

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/ekho/gobackup-docker/internal/labels"
)

// ResolvedCred is a credential the pipeline must materialize: it rendered a
// ${Var} placeholder into the config, and the value comes from Ref (an env var
// name or a file path) inside the client container ContainerID.
type ResolvedCred struct {
	Var         string
	Kind        labels.CredKind
	Ref         string
	ContainerID string
}

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
	Container   string // container name (primary name, e.g. "nextcloud-db")
	ContainerID string // Docker container ID, for inspection during volume discovery
	Name        string // gobackup.name ("" => auto)
	Profile     string // "" => "default"
	Model       map[string]any
	CredRefs    []labels.CredRef // credentials sourced from env/file (_env/_file labels)
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

// Build assembles the full gobackup config document. See BuildWithCreds; this
// wrapper drops the resolved-credential list for callers that don't need it.
func Build(sources []Source, profiles Profiles, hostID, instance string) map[string]any {
	cfg, _ := BuildWithCreds(sources, profiles, hostID, instance)
	return cfg
}

// BuildWithCreds is Build plus credential resolution: for each container it
// deep-merges the labels over the inherited profile, applies "!none" opt-outs,
// expands templates, substitutes ${VAR} placeholders for _env/_file credentials,
// validates, and keys the result by ModelName. It returns the config document
// and the list of ResolvedCred the pipeline must materialize. Invalid, colliding,
// or credential-conflicting models are skipped with a log (fail-closed).
func BuildWithCreds(sources []Source, profiles Profiles, hostID, instance string) (map[string]any, []ResolvedCred) {
	models := map[string]any{}
	var creds []ResolvedCred

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
		m := coerceTree(expanded).(map[string]any)

		modelCreds, err := substituteCreds(m, src.CredRefs, name, src.ContainerID)
		if err != nil {
			log.Printf("[render] model %q: credential error: %v; skipping", name, err)
			continue
		}

		if err := validateModel(m); err != nil {
			log.Printf("[render] model %q invalid: %v; skipping", name, err)
			continue
		}
		if _, dup := models[name]; dup {
			log.Printf("[render] model name collision %q (container %q); skipping duplicate", name, src.Container)
			continue
		}
		models[name] = m
		creds = append(creds, modelCreds...)
	}
	return map[string]any{"models": models}, creds
}

// substituteCreds replaces each credential reference's value in the model with a
// ${VAR} placeholder and returns the ResolvedCreds. It fails (skipping the model)
// if a credential is set both inline and via _env/_file, or if the reference is empty.
func substituteCreds(model map[string]any, refs []labels.CredRef, modelName, containerID string) ([]ResolvedCred, error) {
	var out []ResolvedCred
	for _, r := range refs {
		dotted := strings.Join(r.Path, ".")
		if r.Ref == "" {
			return nil, fmt.Errorf("credential %s has an empty %s reference", dotted, r.Kind)
		}
		if hasLeaf(model, r.Path) {
			return nil, fmt.Errorf("credential %s set both inline and via _%s", dotted, r.Kind)
		}
		v := credVarName(modelName, r.Path)
		setLeaf(model, r.Path, "${"+v+"}")
		out = append(out, ResolvedCred{Var: v, Kind: r.Kind, Ref: r.Ref, ContainerID: containerID})
	}
	return out, nil
}

// credVarName derives a collision-proof, shell-valid env var name from the model
// name and the credential's full path, e.g. GB_NEXTCLOUD_H_DATABASES_DB_PASSWORD.
func credVarName(modelName string, path []string) string {
	parts := append([]string{"GB", modelName}, path...)
	for i, p := range parts {
		parts[i] = sanitizeVar(p)
	}
	return strings.Join(parts, "_")
}

func sanitizeVar(s string) string {
	b := []byte(strings.ToUpper(s))
	for i, c := range b {
		if !((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			b[i] = '_'
		}
	}
	return string(b)
}

func hasLeaf(m map[string]any, path []string) bool {
	cur := m
	for i, k := range path {
		v, ok := cur[k]
		if !ok {
			return false
		}
		if i == len(path)-1 {
			return true
		}
		if cur, ok = v.(map[string]any); !ok {
			return false
		}
	}
	return false
}

func setLeaf(m map[string]any, path []string, val any) {
	cur := m
	for _, k := range path[:len(path)-1] {
		child, ok := cur[k].(map[string]any)
		if !ok {
			child = map[string]any{}
			cur[k] = child
		}
		cur = child
	}
	cur[path[len(path)-1]] = val
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
