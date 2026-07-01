package render

import (
	"reflect"
	"testing"

	"github.com/ekho/gobackup-docker/internal/labels"
)

func TestDeepCopy_isolation(t *testing.T) {
	src := map[string]any{
		"a": map[string]any{"b": 1},
		"s": []any{map[string]any{"x": 1}},
	}
	dst := deepCopy(src).(map[string]any)
	// Mutate the copy deeply; src must be untouched.
	dst["a"].(map[string]any)["b"] = 999
	dst["s"].([]any)[0].(map[string]any)["x"] = 999

	if got := src["a"].(map[string]any)["b"]; got != 1 {
		t.Errorf("deepCopy did not isolate nested map: src mutated to %v", got)
	}
	if got := src["s"].([]any)[0].(map[string]any)["x"]; got != 1 {
		t.Errorf("deepCopy did not isolate nested slice element: src mutated to %v", got)
	}
}

func TestDeepMerge(t *testing.T) {
	base := map[string]any{
		"keep":     10,
		"storages": map[string]any{"local": map[string]any{"keep": 10, "type": "local"}},
	}
	overlay := map[string]any{
		"keep":     "90", // scalar overrides scalar (overlay wins)
		"storages": map[string]any{"local": map[string]any{"keep": "30"}, "s3": map[string]any{"type": "s3"}},
		"new":      "x",
	}
	deepMerge(base, overlay)

	want := map[string]any{
		"keep": "90",
		"storages": map[string]any{
			"local": map[string]any{"keep": "30", "type": "local"}, // merged, type preserved
			"s3":    map[string]any{"type": "s3"},                  // added
		},
		"new": "x",
	}
	if !reflect.DeepEqual(base, want) {
		t.Errorf("deepMerge mismatch:\n got  = %#v\n want = %#v", base, want)
	}
	// overlay must not be aliased into base (deepMerge deep-copies scalars/maps in).
	overlay["storages"].(map[string]any)["s3"].(map[string]any)["type"] = "MUTATED"
	if base["storages"].(map[string]any)["s3"].(map[string]any)["type"] != "s3" {
		t.Error("deepMerge aliased overlay into base")
	}
}

func TestDeepMerge_scalarOverMap(t *testing.T) {
	base := map[string]any{"notifiers": map[string]any{"tg": map[string]any{"type": "telegram"}}}
	deepMerge(base, map[string]any{"notifiers": labels.OptOut})
	if base["notifiers"] != labels.OptOut {
		t.Errorf("scalar should replace map, got %#v", base["notifiers"])
	}
}

func TestPruneOptOut(t *testing.T) {
	m := map[string]any{
		"notifiers": labels.OptOut,
		"storages": map[string]any{
			"local": labels.OptOut,
			"s3":    map[string]any{"type": "s3"},
		},
		"schedule": map[string]any{"cron": "0 1 * * *"},
	}
	pruneOptOut(m)
	want := map[string]any{
		"storages": map[string]any{"s3": map[string]any{"type": "s3"}},
		"schedule": map[string]any{"cron": "0 1 * * *"},
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("pruneOptOut mismatch:\n got  = %#v\n want = %#v", m, want)
	}
}

func TestExpandTemplates(t *testing.T) {
	data := TemplateData{Model: "gitea-h1", Container: "gitea", Host: "h1", Instance: "prod"}
	in := map[string]any{
		"path":   "/backups/{{ .Model }}",
		"secret": "${YC_SECRET}", // no template tokens: must pass through untouched
		"list":   []any{"a-{{ .Host }}", "plain"},
		"num":    10, // non-string untouched
	}
	out, err := expandTemplates(in, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := out.(map[string]any)
	if m["path"] != "/backups/gitea-h1" {
		t.Errorf("path = %q", m["path"])
	}
	if m["secret"] != "${YC_SECRET}" {
		t.Errorf("secret should be untouched, got %q", m["secret"])
	}
	if got := m["list"].([]any)[0]; got != "a-h1" {
		t.Errorf("list[0] = %q", got)
	}
	if m["num"] != 10 {
		t.Errorf("num = %v", m["num"])
	}
}

func TestExpandTemplates_errors(t *testing.T) {
	data := TemplateData{Model: "m"}
	for _, bad := range []string{"{{ .Model", "{{ .Nonexistent }}"} {
		if _, err := expandTemplates(map[string]any{"k": bad}, data); err == nil {
			t.Errorf("expected error for template %q", bad)
		}
	}
}
