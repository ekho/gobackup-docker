package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekho/gobackup-docker/internal/apply"
	"github.com/ekho/gobackup-docker/internal/docker"
	"gopkg.in/yaml.v3"
)

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
			"gobackup.enable":               "true",
			"gobackup.databases.main.type":  "postgresql",
			"gobackup.databases.main.host":  "pg",
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
