package pipeline

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"

	"github.com/ekho/gobackup-docker/internal/docker"
)

// fakeInspector returns canned inspect results for testing.
type fakeInspector struct {
	results map[string]docker.InspectResult // container ID → inspect result
}

func (f *fakeInspector) ContainerInspect(_ context.Context, id string) (docker.InspectResult, error) {
	return f.results[id], nil
}

func mpVolume(name, dest string) container.MountPoint {
	return container.MountPoint{
		Type:        mount.TypeVolume,
		Name:        name,
		Destination: dest,
	}
}

func mpBind(source, dest string) container.MountPoint {
	return container.MountPoint{
		Type:        mount.TypeBind,
		Source:      source,
		Destination: dest,
	}
}

func TestMergeMounts_preservesBaseDropsStaleArchiveAddsNew(t *testing.T) {
	existing := []container.MountPoint{
		mpVolume("cfg", "/etc/gobackup"),
		mpVolume("backups", "/backups"),
		mpVolume("stale", "/volumes/old/data"), // previously-managed archive → must be dropped
	}
	archive := []docker.MountDef{
		{Type: mount.TypeVolume, Source: "html_data", Target: "/volumes/myapp/var/www/html", ReadOnly: true},
	}
	got := mergeMounts(existing, archive)

	byTarget := map[string]docker.MountDef{}
	for _, m := range got {
		byTarget[m.Target] = m
	}
	if _, ok := byTarget["/etc/gobackup"]; !ok {
		t.Error("config volume must be preserved (else recreated gobackup can't read its config)")
	}
	if _, ok := byTarget["/backups"]; !ok {
		t.Error("backups volume must be preserved")
	}
	if _, ok := byTarget["/volumes/old/data"]; ok {
		t.Error("stale archive mount must be dropped")
	}
	if _, ok := byTarget["/volumes/myapp/var/www/html"]; !ok {
		t.Error("new archive mount must be added")
	}
	if len(got) != 3 {
		t.Errorf("want 3 mounts (cfg, backups, new archive), got %d: %#v", len(got), got)
	}
	if byTarget["/etc/gobackup"].Source != "cfg" {
		t.Errorf("preserved volume should keep its name as source: %#v", byTarget["/etc/gobackup"])
	}
}

func TestMergeMounts_emptyArchiveKeepsBase(t *testing.T) {
	got := mergeMounts([]container.MountPoint{mpVolume("cfg", "/etc/gobackup")}, nil)
	if len(got) != 1 || got[0].Target != "/etc/gobackup" {
		t.Errorf("got %#v, want [cfg→/etc/gobackup]", got)
	}
}

func TestDiscoverArchiveVolumes_emptyIncludes(t *testing.T) {
	inspector := &fakeInspector{}
	av, err := discoverArchiveVolumes(context.Background(), inspector, "c1", "m1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(av.Mounts) != 0 || len(av.Includes) != 0 {
		t.Errorf("expected empty, got mounts=%d includes=%d", len(av.Mounts), len(av.Includes))
	}
}

func TestDiscoverArchiveVolumes_namedVolume(t *testing.T) {
	inspector := &fakeInspector{results: map[string]docker.InspectResult{
		"c1": {Mounts: []container.MountPoint{mpVolume("html_data", "/var/www/html")}},
	}}
	av, err := discoverArchiveVolumes(context.Background(), inspector, "c1", "m1",
		[]string{"/var/www/html"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(av.Mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(av.Mounts))
	}
	m := av.Mounts[0]
	if m.Source != "html_data" {
		t.Errorf("source = %q, want html_data", m.Source)
	}
	if m.Target != "/volumes/m1/var/www/html" {
		t.Errorf("target = %q", m.Target)
	}
	if !m.ReadOnly {
		t.Error("mount should be read-only")
	}
	if len(av.Includes) != 1 || av.Includes[0] != "/volumes/m1/var/www/html" {
		t.Errorf("includes = %#v", av.Includes)
	}
}

func TestDiscoverArchiveVolumes_bindMount(t *testing.T) {
	inspector := &fakeInspector{results: map[string]docker.InspectResult{
		"c1": {Mounts: []container.MountPoint{
			mpBind("/host/data", "/data"),
		}},
	}}
	av, err := discoverArchiveVolumes(context.Background(), inspector, "c1", "m1",
		[]string{"/data"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(av.Mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(av.Mounts))
	}
	m := av.Mounts[0]
	if m.Source != "/host/data" {
		t.Errorf("source = %q, want /host/data", m.Source)
	}
	if m.Target != "/volumes/m1/data" {
		t.Errorf("target = %q", m.Target)
	}
}

func TestDiscoverArchiveVolumes_subdirPath(t *testing.T) {
	inspector := &fakeInspector{results: map[string]docker.InspectResult{
		"c1": {Mounts: []container.MountPoint{
			mpVolume("html_data", "/var/www/html"),
		}},
	}}
	av, err := discoverArchiveVolumes(context.Background(), inspector, "c1", "m1",
		[]string{"/var/www/html/uploads"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(av.Mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(av.Mounts))
	}
	// Mount target should be the mount point, not the subdir.
	if av.Mounts[0].Target != "/volumes/m1/var/www/html" {
		t.Errorf("target = %q", av.Mounts[0].Target)
	}
	// But the include path should be the full transformed subdir path.
	if len(av.Includes) != 1 || av.Includes[0] != "/volumes/m1/var/www/html/uploads" {
		t.Errorf("includes = %#v", av.Includes)
	}
}

func TestDiscoverArchiveVolumes_multipleMounts(t *testing.T) {
	inspector := &fakeInspector{results: map[string]docker.InspectResult{
		"c1": {Mounts: []container.MountPoint{
			mpVolume("html_data", "/var/www/html"),
			mpVolume("config_data", "/etc/nginx"),
		}},
	}}
	av, err := discoverArchiveVolumes(context.Background(), inspector, "c1", "m1",
		[]string{"/var/www/html", "/etc/nginx/sites-enabled"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(av.Mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(av.Mounts))
	}
	if len(av.Includes) != 2 {
		t.Fatalf("expected 2 includes, got %d", len(av.Includes))
	}
	if av.Includes[0] != "/volumes/m1/var/www/html" {
		t.Errorf("includes[0] = %q", av.Includes[0])
	}
	if av.Includes[1] != "/volumes/m1/etc/nginx/sites-enabled" {
		t.Errorf("includes[1] = %q", av.Includes[1])
	}
}

func TestDiscoverArchiveVolumes_unmatchedPath(t *testing.T) {
	inspector := &fakeInspector{results: map[string]docker.InspectResult{
		"c1": {Mounts: []container.MountPoint{
			mpVolume("html_data", "/var/www/html"),
		}},
	}}
	av, err := discoverArchiveVolumes(context.Background(), inspector, "c1", "m1",
		[]string{"/var/www/html", "/nonexistent/path"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The unmatched path should be silently skipped (logged), the valid one kept.
	if len(av.Includes) != 1 {
		t.Fatalf("expected 1 include (unmatched skipped), got %d: %#v", len(av.Includes), av.Includes)
	}
	if av.Includes[0] != "/volumes/m1/var/www/html" {
		t.Errorf("includes[0] = %q", av.Includes[0])
	}
}

func TestApplyArchiveVolumes_mergesAndDedups(t *testing.T) {
	models := map[string]any{
		"m1": map[string]any{"archive": map[string]any{"includes": []any{"/old/m1/path"}}},
		"m2": map[string]any{"archive": map[string]any{"includes": []any{"/old/m2/path"}}},
	}

	vols := []ArchiveVolumes{
		{
			ModelName: "m1",
			Mounts:    []docker.MountDef{{Source: "v1", Target: "/volumes/m1/a"}},
			Includes:  []string{"/volumes/m1/a"},
		},
		{
			ModelName: "m2",
			Mounts:    []docker.MountDef{{Source: "v2", Target: "/volumes/m2/b"}},
			Includes:  []string{"/volumes/m2/b"},
		},
	}

	mounts := applyArchiveVolumes(models, vols)
	if len(mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(mounts))
	}

	m1 := models["m1"].(map[string]any)["archive"].(map[string]any)
	if got := m1["includes"].([]string)[0]; got != "/volumes/m1/a" {
		t.Errorf("m1 include = %q, want /volumes/m1/a", got)
	}
}

func TestApplyArchiveVolumes_dedupSameMount(t *testing.T) {
	models := map[string]any{
		"m1": map[string]any{},
		"m2": map[string]any{},
	}
	vols := []ArchiveVolumes{
		{
			ModelName: "m1",
			Mounts:    []docker.MountDef{{Source: "shared_vol", Target: "/volumes/m1/path"}},
			Includes:  []string{"/volumes/m1/path"},
		},
		{
			ModelName: "m2",
			Mounts:    []docker.MountDef{{Source: "shared_vol", Target: "/volumes/m1/path"}},
			Includes:  []string{"/volumes/m2/other"},
		},
	}
	mounts := applyArchiveVolumes(models, vols)
	if len(mounts) != 1 {
		t.Fatalf("expected 1 deduplicated mount, got %d", len(mounts))
	}
}
