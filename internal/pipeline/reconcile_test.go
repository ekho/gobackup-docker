package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/ekho/gobackup-docker/internal/apply"
	gbcontainer "github.com/ekho/gobackup-docker/internal/container"
	"github.com/ekho/gobackup-docker/internal/docker"
	"github.com/ekho/gobackup-docker/internal/labels"
	"github.com/ekho/gobackup-docker/internal/render"
	"gopkg.in/yaml.v3"
)

func TestResolveCreds(t *testing.T) {
	cm := &fakeContainerManager{
		results: map[string]docker.InspectResult{
			"c1": {
				Env:    []string{"DB_PW=secret", "OTHER=x"},
				Mounts: []container.MountPoint{mpBind("/host/sk", "/run/secrets/sk")},
			},
		},
	}
	creds := []render.ResolvedCred{
		{Var: "GB_A", Kind: labels.CredEnv, Ref: "DB_PW", ContainerID: "c1"},
		{Var: "GB_B", Kind: labels.CredFile, Ref: "/run/secrets/sk", ContainerID: "c1"},
		{Var: "GB_C", Kind: labels.CredEnv, Ref: "MISSING", ContainerID: "c1"}, // skipped
	}
	out := resolveCreds(context.Background(), cm, creds)

	if len(out.envVars) != 1 || out.envVars[0] != "GB_A=secret" {
		t.Errorf("envVars = %#v, want [GB_A=secret]", out.envVars)
	}
	if len(out.secretMounts) != 1 {
		t.Fatalf("secretMounts = %#v", out.secretMounts)
	}
	if m := out.secretMounts[0]; m.Source != "/host/sk" || m.Target != "/gobackup-secrets/GB_B" || !m.ReadOnly {
		t.Errorf("secret mount = %#v, want /host/sk → /gobackup-secrets/GB_B ro", m)
	}
	if len(out.secretExports) != 1 || out.secretExports[0] != (secretExport{Var: "GB_B", Path: "/gobackup-secrets/GB_B"}) {
		t.Errorf("secretExports = %#v", out.secretExports)
	}
}

type fakeLister struct {
	containers []docker.Container
	err        error
	calls      atomic.Int64
}

func (f *fakeLister) List(context.Context) ([]docker.Container, error) {
	f.calls.Add(1)
	return f.containers, f.err
}

func writeDefaults(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "defaults.yml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const defaultsProfile = `
default:
  compress_with:
    type: tgz
  default_storage: local
  storages:
    local:
      type: local
      keep: 5
      path: /b/{{ .Model }}
`

// readModels runs one reconcile and returns the models map from the written file.
func readModels(t *testing.T, r *Reconciler, outPath string) map[string]any {
	t.Helper()
	if err := r.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse output: %v", err)
	}
	m, _ := doc["models"].(map[string]any)
	return m
}

func TestReconcile_gatingAndRender(t *testing.T) {
	defaults := writeDefaults(t, defaultsProfile)
	out := filepath.Join(t.TempDir(), "gobackup.yml")

	lister := &fakeLister{containers: []docker.Container{
		{ID: "1", Name: "pg", Labels: map[string]string{
			"gobackup.enable":              "true",
			"gobackup.databases.main.type": "postgresql",
			"gobackup.databases.main.host": "pg",
		}},
		{ID: "2", Name: "off", Labels: map[string]string{"gobackup.enable": "false",
			"gobackup.databases.x.type": "mysql"}},
		{ID: "3", Name: "nolabels", Labels: map[string]string{}},
	}}
	r := NewReconciler(Config{DefaultsPath: defaults, HostID: "h"}, lister, &apply.FileWriter{Path: out})

	models := readModels(t, r, out)
	if len(models) != 1 {
		t.Fatalf("expected 1 model (only the enabled one), got %d: %#v", len(models), models)
	}
	pg, ok := models["pg-h"].(map[string]any) // auto name <container>-<host>
	if !ok {
		t.Fatalf("model pg-h missing: %#v", models)
	}
	// inherited from profile + template expanded:
	if got := pg["storages"].(map[string]any)["local"].(map[string]any)["path"]; got != "/b/pg-h" {
		t.Errorf("path = %v", got)
	}
	if pg["databases"].(map[string]any)["main"].(map[string]any)["type"] != "postgresql" {
		t.Errorf("db type wrong: %#v", pg["databases"])
	}
}

func TestReconcile_instanceScope(t *testing.T) {
	defaults := writeDefaults(t, defaultsProfile)
	out := filepath.Join(t.TempDir(), "gobackup.yml")
	lister := &fakeLister{containers: []docker.Container{
		{Name: "a", Labels: map[string]string{"gobackup.enable": "true", "gobackup.instance": "prod",
			"gobackup.databases.d.type": "postgresql"}},
		{Name: "b", Labels: map[string]string{"gobackup.enable": "true", "gobackup.instance": "staging",
			"gobackup.databases.d.type": "postgresql"}},
		{Name: "c", Labels: map[string]string{"gobackup.enable": "true", // unscoped: managed by any instance
			"gobackup.databases.d.type": "postgresql"}},
	}}
	r := NewReconciler(Config{DefaultsPath: defaults, HostID: "h", Instance: "prod"}, lister, &apply.FileWriter{Path: out})

	models := readModels(t, r, out)
	if _, ok := models["a-h"]; !ok {
		t.Error("prod-scoped container should be included")
	}
	if _, ok := models["b-h"]; ok {
		t.Error("staging-scoped container must be excluded for instance=prod")
	}
	if _, ok := models["c-h"]; !ok {
		t.Error("unscoped container should be included")
	}
}

func TestReconcile_keepsLastGoodOnBadDefaults(t *testing.T) {
	defaults := writeDefaults(t, defaultsProfile)
	out := filepath.Join(t.TempDir(), "gobackup.yml")
	lister := &fakeLister{containers: []docker.Container{
		{Name: "pg", Labels: map[string]string{"gobackup.enable": "true", "gobackup.databases.d.type": "postgresql"}},
	}}
	r := NewReconciler(Config{DefaultsPath: defaults, HostID: "h"}, lister, &apply.FileWriter{Path: out})

	// First good reconcile.
	if _, ok := readModels(t, r, out)["pg-h"]; !ok {
		t.Fatal("first reconcile should produce pg-h")
	}
	good, _ := os.ReadFile(out)

	// Corrupt defaults.yml; reconcile must error and NOT touch the file.
	if err := os.WriteFile(defaults, []byte("default: [unclosed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcile(context.Background()); err == nil {
		t.Error("expected error on malformed defaults")
	}
	after, _ := os.ReadFile(out)
	if string(after) != string(good) {
		t.Error("last-good config was clobbered on bad defaults")
	}
}

func TestRun_debounceCoalesces(t *testing.T) {
	defaults := writeDefaults(t, defaultsProfile)
	out := filepath.Join(t.TempDir(), "gobackup.yml")
	lister := &fakeLister{} // empty: reconcile still runs (and counts) but writes empty models
	r := NewReconciler(Config{DefaultsPath: defaults, HostID: "h", Debounce: 60 * time.Millisecond}, lister, &apply.FileWriter{Path: out})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	trigger := make(chan struct{}, 1)
	go r.Run(ctx, trigger)

	// Burst: many triggers well within one debounce window → one reconcile.
	for i := 0; i < 8; i++ {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}
	time.Sleep(200 * time.Millisecond)
	if got := lister.calls.Load(); got != 1 {
		t.Fatalf("burst should coalesce to 1 reconcile, got %d", got)
	}

	// A later trigger → a second reconcile.
	trigger <- struct{}{}
	time.Sleep(200 * time.Millisecond)
	if got := lister.calls.Load(); got != 2 {
		t.Fatalf("expected 2 reconciles after second trigger, got %d", got)
	}
}

// fakeContainerManager returns canned inspect results for archive volume tests.
type fakeContainerManager struct {
	results     map[string]docker.InspectResult
	all         []docker.Container
	createdSpec *docker.ContainerSpec // last spec passed to ContainerCreate
}

func (f *fakeContainerManager) ContainerInspect(_ context.Context, id string) (docker.InspectResult, error) {
	return f.results[id], nil
}
func (f *fakeContainerManager) ContainerCreate(_ context.Context, spec docker.ContainerSpec) (string, error) {
	f.createdSpec = &spec
	return "new-id", nil
}
func (f *fakeContainerManager) ContainerStart(_ context.Context, _ string) error          { return nil }
func (f *fakeContainerManager) ContainerStop(_ context.Context, _ string, _ *int) error   { return nil }
func (f *fakeContainerManager) ContainerRemove(_ context.Context, _ string, _ bool) error { return nil }
func (f *fakeContainerManager) ListAll(_ context.Context) ([]docker.Container, error) {
	return f.all, nil
}

func TestReconcile_archiveVolumes(t *testing.T) {
	defaults := writeDefaults(t, defaultsProfile)
	out := filepath.Join(t.TempDir(), "gobackup.yml")

	lister := &fakeLister{containers: []docker.Container{
		{
			ID:   "c1",
			Name: "app",
			Labels: map[string]string{
				"gobackup.enable":            "true",
				"gobackup.name":              "myapp",
				"gobackup.archive.includes":  "/var/www/html,/etc/nginx",
				"gobackup.archive.excludes":  "*.log",
				"gobackup.databases.db.type": "postgresql",
				"gobackup.databases.db.host": "app-db",
			},
		},
	}}

	cm := &fakeContainerManager{
		results: map[string]docker.InspectResult{
			"c1": {Mounts: []container.MountPoint{
				mpVolume("html_data", "/var/www/html"),
				mpVolume("nginx_cfg", "/etc/nginx"),
			}},
		},
	}

	r := NewReconciler(Config{DefaultsPath: defaults, HostID: "h"}, lister, &apply.FileWriter{Path: out})
	r.WithContainerManager(cm)

	models := readModels(t, r, out)
	m, ok := models["myapp"].(map[string]any)
	if !ok {
		t.Fatalf("model 'myapp' not found: %#v", models)
	}

	arch, ok := m["archive"].(map[string]any)
	if !ok {
		t.Fatal("model missing archive block")
	}

	includesRaw := arch["includes"].([]any)
	if len(includesRaw) != 2 {
		t.Fatalf("expected 2 includes, got %d: %#v", len(includesRaw), includesRaw)
	}
	if includesRaw[0].(string) != "/volumes/myapp/var/www/html" {
		t.Errorf("includes[0] = %q", includesRaw[0])
	}
	if includesRaw[1].(string) != "/volumes/myapp/etc/nginx" {
		t.Errorf("includes[1] = %q", includesRaw[1])
	}

	excludesRaw := arch["excludes"].([]any)
	if len(excludesRaw) != 1 || excludesRaw[0].(string) != "*.log" {
		t.Errorf("excludes = %#v", excludesRaw)
	}
}

func TestReconcile_recreateUsesGobackupSpec(t *testing.T) {
	// End-to-end wiring: WithGobackupSpec (from the supervisor's own labels) must
	// shape the recreated gobackup container, not the existing container's image.
	defaults := writeDefaults(t, defaultsProfile)
	out := filepath.Join(t.TempDir(), "gobackup.yml")

	lister := &fakeLister{containers: []docker.Container{{
		ID:   "c1",
		Name: "app",
		Labels: map[string]string{
			"gobackup.enable":           "true",
			"gobackup.name":             "myapp",
			"gobackup.archive.includes": "/var/www/html",
		},
	}}}

	cm := &fakeContainerManager{
		all: []docker.Container{
			{ID: "gb1", Name: "gobackup", Labels: map[string]string{gobackupComponentLabel: gobackupComponentValue}},
		},
		results: map[string]docker.InspectResult{
			"c1": {Mounts: []container.MountPoint{mpVolume("html_data", "/var/www/html")}},
			"gb1": {
				ID:     "gb1",
				Name:   "gobackup",
				Image:  "orig/image:1",
				Labels: map[string]string{gobackupComponentLabel: gobackupComponentValue},
				Mounts: nil, // differs from desired → triggers recreate
			},
		},
	}

	r := NewReconciler(Config{DefaultsPath: defaults, HostID: "h"}, lister, &apply.FileWriter{Path: out}).
		WithContainerManager(cm).
		WithGobackupSpec(gbcontainer.Config{
			Image:   "custom/gobackup:9",
			Command: "/usr/local/bin/gobackup run -c /x",
		})

	if err := r.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if cm.createdSpec == nil {
		t.Fatal("gobackup container was not recreated")
	}
	if cm.createdSpec.Image != "custom/gobackup:9" {
		t.Errorf("recreated Image = %q, want custom/gobackup:9 (from gobackup_container.image, not existing)", cm.createdSpec.Image)
	}
	if got := cm.createdSpec.Command; len(got) != 4 || got[0] != "/usr/local/bin/gobackup" {
		t.Errorf("recreated Command = %#v, want full argv from label", got)
	}
	if cm.createdSpec.Labels[gobackupComponentLabel] != gobackupComponentValue {
		t.Errorf("recreated container lost the component label (unfindable next reconcile): %#v", cm.createdSpec.Labels)
	}
}

func TestWatchFile_firesOnWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "defaults.yml")
	if err := os.WriteFile(path, []byte("a: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fired := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go WatchFile(ctx, path, func() { fired <- struct{}{} })

	time.Sleep(80 * time.Millisecond) // let the watcher establish
	if err := os.WriteFile(path, []byte("a: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchFile did not fire on write")
	}
}
