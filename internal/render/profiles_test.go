package render

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadProfiles_missingIsEmpty(t *testing.T) {
	p, err := LoadProfiles(filepath.Join(t.TempDir(), "does-not-exist.yml"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(p) != 0 {
		t.Errorf("expected empty profiles, got %#v", p)
	}
}

func TestLoadProfiles_resolvesAnchors(t *testing.T) {
	// The user's real config uses &anchors + <<: merge keys; yaml.v3 must resolve
	// them at parse so they never leak into the label surface.
	yml := `
x-common: &common
  type: local
  keep: 10

default:
  storages:
    local:
      <<: *common
      path: /backups
`
	dir := t.TempDir()
	path := filepath.Join(dir, "defaults.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadProfiles(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	local := p["default"]["storages"].(map[string]any)["local"].(map[string]any)
	if local["type"] != "local" || local["keep"] != 10 || local["path"] != "/backups" {
		t.Errorf("merge key not resolved: %#v", local)
	}
}

func TestLoadProfiles_malformedErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yml")
	if err := os.WriteFile(path, []byte("default:\n  ::: not: valid: yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProfiles(path); err == nil {
		t.Error("expected error for malformed YAML (so caller keeps last-good)")
	}
}
