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

// Parsed is the decoded gobackup.* surface of one container: exactly one model.
type Parsed struct {
	Enabled  bool
	Name     string         // gobackup.name — explicit model name ("" => auto from container+host)
	Instance string         // scope selector ("" = unscoped)
	Profile  string         // defaults.yml profile name ("" => "default")
	Model    map[string]any // the container's single model config tree
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
			setPath(p.Model, strings.Split(rest, "."), v)
		}
	}
	return p
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
