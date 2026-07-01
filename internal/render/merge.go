package render

import (
	"strings"
	"text/template"

	"github.com/ekho/gobackup-docker/internal/labels"
)

// deepCopy clones a decoded YAML/label value so merging never mutates a shared
// profile body.
func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = deepCopy(val)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, val := range t {
			s[i] = deepCopy(val)
		}
		return s
	default:
		return v
	}
}

// deepMerge overlays overlay onto base in place. When both sides hold a map the
// merge recurses; otherwise the overlay value wins (labels beat the profile).
func deepMerge(base, overlay map[string]any) {
	for k, ov := range overlay {
		if bm, ok := base[k].(map[string]any); ok {
			if om, ok := ov.(map[string]any); ok {
				deepMerge(bm, om)
				continue
			}
		}
		base[k] = deepCopy(ov)
	}
}

// pruneOptOut deletes any key whose value is the "!none" sentinel, applied after
// merging so a label can remove a subtree inherited from the profile.
func pruneOptOut(m map[string]any) {
	for k, v := range m {
		switch t := v.(type) {
		case string:
			if t == labels.OptOut {
				delete(m, k)
			}
		case map[string]any:
			pruneOptOut(t)
		}
	}
}

// expandTemplates evaluates Go text/template tokens ({{ .Model }} etc.) over
// every string leaf. This is the supervisor's own substitution pass; it runs
// before the file is written and never touches ${VAR}, which gobackup expands
// later via os.ExpandEnv.
func expandTemplates(v any, data TemplateData) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			nv, err := expandTemplates(val, data)
			if err != nil {
				return nil, err
			}
			t[k] = nv
		}
		return t, nil
	case []any:
		for i, val := range t {
			nv, err := expandTemplates(val, data)
			if err != nil {
				return nil, err
			}
			t[i] = nv
		}
		return t, nil
	case string:
		if !strings.Contains(t, "{{") {
			return t, nil
		}
		tmpl, err := template.New("label").Option("missingkey=error").Parse(t)
		if err != nil {
			return nil, err
		}
		var sb strings.Builder
		if err := tmpl.Execute(&sb, data); err != nil {
			return nil, err
		}
		return sb.String(), nil
	default:
		return v, nil
	}
}
