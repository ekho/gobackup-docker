// Package labels turns a container's flat gobackup.* label map into a single
// model config tree. It is deliberately stateless: it knows nothing about the
// defaults.yml profiles (that coupling belongs in package render), so the same
// dotted grammar is decoded the same way regardless of what profiles exist.
//
// Grammar (one backup model per container):
//
//	gobackup.enable            = "true"          # opt-in gate
//	gobackup.name              = "mynextcloud"   # explicit model name (optional)
//	gobackup.instance          = "prod"          # scope selector (optional)
//	gobackup.profile           = "default"       # defaults.yml profile (optional)
//	gobackup.<config.path>     = "..."           # everything else → the model body,
//	                                             #   e.g. gobackup.databases.nc.type,
//	                                             #        gobackup.archive.includes
package labels

import "strings"

const prefix = "gobackup."

// Reserved single-segment meta keys (gobackup.<key>); everything else is config.
const (
	enableKey   = "enable"
	nameKey     = "name"
	instanceKey = "instance"
	profileKey  = "profile"

	// OptOut is the sentinel value that deletes an inherited subtree, e.g.
	// gobackup.notifiers: "!none". A leading '!' cannot begin a real gobackup
	// scalar, so it is unambiguous. Handled in package render.
	OptOut = "!none"
)

// CredKind is the source of a credential referenced by a label.
type CredKind string

const (
	CredEnv  CredKind = "env"  // value comes from an env var in the client container
	CredFile CredKind = "file" // value comes from a file (e.g. a Docker secret) in the client container
)

// credKeys is the allowlist of credential base-keys eligible for the
// _env/_file suffix. It is intentionally NOT the whole isProtectedKey set:
// keys like `host`/`*_id` aren't secrets, and — critically — `credentials` is
// excluded so gobackup's real `storages.<id>.credentials_file` key (a GCS
// keyfile path gobackup reads itself) is never hijacked as a credential ref.
var credKeys = map[string]bool{
	"password":   true,
	"token":      true,
	"secret":     true,
	"access_key": true,
	"secret_key": true,
}

// CredRef is a credential whose value is sourced indirectly (from an env var or
// a file) rather than written inline. Path is the location of the credential key
// in the model tree (e.g. ["databases","nc","password"]); render substitutes a
// ${VAR} placeholder there and the pipeline resolves Ref into that VAR.
type CredRef struct {
	Path []string
	Kind CredKind
	Ref  string // env var name (Kind=env) or file path (Kind=file)
}

// Parsed is the decoded gobackup.* surface of one container: exactly one model.
type Parsed struct {
	Enabled  bool
	Name     string         // gobackup.name — explicit model name ("" => auto from container+host)
	Instance string         // scope selector ("" = unscoped)
	Profile  string         // defaults.yml profile name ("" => "default")
	Model    map[string]any // the container's single model config tree
	CredRefs []CredRef      // credentials sourced from env/file (_env/_file suffix labels)
}

// Parse decodes gobackup.* labels. exposedByDefault decides inclusion when the
// container carries no gobackup.enable label.
func Parse(labels map[string]string, exposedByDefault bool) Parsed {
	p := Parsed{Enabled: exposedByDefault, Model: map[string]any{}}
	for k, v := range labels {
		rest, ok := strings.CutPrefix(k, prefix)
		if !ok || rest == "" {
			continue
		}
		switch rest {
		case enableKey:
			p.Enabled = parseBool(v, exposedByDefault)
		case nameKey:
			p.Name = v
		case instanceKey:
			p.Instance = v
		case profileKey:
			p.Profile = v
		default:
			parts := strings.Split(rest, ".")
			if ref, ok := credRef(parts, v); ok {
				p.CredRefs = append(p.CredRefs, ref)
				continue // credential ref: not a literal model value
			}
			setPath(p.Model, parts, v)
		}
	}
	return p
}

// credRef recognises a credential-source label whose last segment is
// <credkey>_env or <credkey>_file with <credkey> in the allowlist, e.g.
// databases.nc.password_env. Returns the CredRef pointing at the credential key
// (…password) with its kind and reference value.
func credRef(parts []string, value string) (CredRef, bool) {
	if len(parts) < 2 {
		return CredRef{}, false
	}
	last := parts[len(parts)-1]
	base, kind, ok := credentialSuffix(last)
	if !ok {
		return CredRef{}, false
	}
	path := append(append([]string(nil), parts[:len(parts)-1]...), base)
	return CredRef{Path: path, Kind: kind, Ref: value}, true
}

func credentialSuffix(seg string) (base string, kind CredKind, ok bool) {
	if b, found := strings.CutSuffix(seg, "_env"); found && credKeys[b] {
		return b, CredEnv, true
	}
	if b, found := strings.CutSuffix(seg, "_file"); found && credKeys[b] {
		return b, CredFile, true
	}
	return "", "", false
}

// setPath assigns val at a nested path, creating intermediate maps. If an
// intermediate key already holds a scalar, it is overwritten with a map (last
// write wins) — acceptable for the label surface.
func setPath(tree map[string]any, path []string, val any) {
	for _, key := range path[:len(path)-1] {
		child, ok := tree[key].(map[string]any)
		if !ok {
			child = map[string]any{}
			tree[key] = child
		}
		tree = child
	}
	tree[path[len(path)-1]] = val
}

// parseBool leniently interprets label booleans (Compose passes strings).
func parseBool(v string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on", "enable", "enabled":
		return true
	case "false", "0", "no", "off", "disable", "disabled":
		return false
	default:
		return fallback
	}
}
