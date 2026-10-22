package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/errdefs"

	"github.com/ekho/gobackup-docker/internal/apply"
	gbcontainer "github.com/ekho/gobackup-docker/internal/container"
	"github.com/ekho/gobackup-docker/internal/docker"
)

// recoveryDocker models Docker's state independently of the reconciler, including
// operations that succeed on the daemon while the caller receives an error.
type recoveryDocker struct {
	engine         *docker.InspectResult
	failAt         string
	running        bool
	creates, stops int
}

func newRecoveryDocker() *recoveryDocker {
	e := existingFixture()
	e.Mounts = []container.MountPoint{mpVolume("config", "/etc/gobackup"), mpVolume("state", "/root/.gobackup")}
	e.Env = []string{"TZ=Europe/Moscow", "TOKEN=synthetic-secret"}
	return &recoveryDocker{engine: &e, running: true}
}

func (d *recoveryDocker) ContainerInspect(_ context.Context, id string) (docker.InspectResult, error) {
	if id == "app" {
		return docker.InspectResult{Mounts: []container.MountPoint{mpVolume("data", "/data")}}, nil
	}
	if d.engine != nil && (id == d.engine.ID || id == d.engine.Name) {
		return *d.engine, nil
	}
	return docker.InspectResult{}, errdefs.NotFound(errors.New("container missing"))
}

func (d *recoveryDocker) ListAll(context.Context) ([]docker.Container, error) {
	if d.engine == nil {
		return nil, nil
	}
	return []docker.Container{{ID: d.engine.ID, Name: d.engine.Name, Labels: d.engine.Labels}}, nil
}

func (d *recoveryDocker) ContainerStop(context.Context, string, *int) error {
	d.stops++
	if d.failAt == "stop-disappeared" {
		d.failAt = ""
		d.engine = nil
		return errdefs.NotFound(errors.New("container removed concurrently"))
	}
	if !d.running {
		return errdefs.NotModified(errors.New("already stopped"))
	}
	d.running = false
	return nil
}

func (d *recoveryDocker) ContainerRemove(context.Context, string, bool) error {
	if d.engine == nil {
		return errdefs.NotFound(errors.New("already removed"))
	}
	if d.failAt == "remove-in-progress" {
		d.failAt = ""
		return errdefs.Conflict(errors.New("removal of container is already in progress"))
	}
	d.engine = nil
	if d.failAt == "remove" {
		d.failAt = ""
		return errdefs.Conflict(errors.New("removal of container is already in progress"))
	}
	return nil
}

func (d *recoveryDocker) ContainerCreate(_ context.Context, s docker.ContainerSpec) (string, error) {
	if d.engine != nil {
		return "", errdefs.Conflict(errors.New("name already in use"))
	}
	if d.failAt == "create" {
		d.failAt = ""
		return "", errors.New("daemon unavailable")
	}
	d.creates++
	e := docker.InspectResult{ID: fmt.Sprintf("replacement-%d", d.creates), Name: s.Name,
		Image: s.Image, Command: s.Command, Env: s.Env, Labels: s.Labels,
		Networks: map[string]*network.EndpointSettings{}}
	for _, n := range s.Networks {
		e.Networks[n] = &network.EndpointSettings{}
	}
	for _, m := range s.Mounts {
		e.Mounts = append(e.Mounts, container.MountPoint{Type: m.Type, Name: m.Source, Source: m.Source, Destination: m.Target, RW: !m.ReadOnly})
	}
	d.engine = &e
	d.running = false
	if d.failAt == "create-response" {
		d.failAt = ""
		return "", errors.New("create response lost")
	}
	return e.ID, nil
}

func (d *recoveryDocker) ContainerStart(_ context.Context, id string) error {
	if d.engine == nil || d.engine.ID != id {
		return errdefs.NotFound(errors.New("container missing"))
	}
	if d.failAt == "start" {
		d.failAt = ""
		return errors.New("start temporarily failed")
	}
	if d.running {
		return errdefs.NotModified(errors.New("already running"))
	}
	d.running = true
	if d.failAt == "start-response" {
		d.failAt = ""
		return errors.New("start response lost")
	}
	return nil
}

func recoveryReconciler(t *testing.T, d ContainerManager, output string) *Reconciler {
	t.Helper()
	lister := &fakeLister{containers: []docker.Container{{ID: "app", Name: "app", Labels: map[string]string{
		"gobackup.enable": "true", "gobackup.name": "app", "gobackup.archive.includes": "/data",
	}}}}
	return NewReconciler(Config{DefaultsPath: writeDefaults(t, defaultsProfile), Debounce: time.Millisecond}, lister,
		&apply.FileWriter{Path: output}).WithContainerManager(d)
}

func TestReconcile_recoversInterruptedReplacement(t *testing.T) {
	for _, stage := range []string{"remove", "remove-in-progress", "create", "create-response", "start", "start-response"} {
		t.Run(stage, func(t *testing.T) {
			d := newRecoveryDocker()
			d.failAt = stage
			output := filepath.Join(t.TempDir(), "gobackup.yml")
			r := recoveryReconciler(t, d, output)
			if err := r.reconcile(context.Background()); err == nil {
				t.Error("interrupted replacement must return an error")
			}
			if r.Status().LastError == "" {
				t.Error("sync failure missing from status")
			}
			// A fresh controller must resume using only persisted state, not RAM.
			r = recoveryReconciler(t, d, output)
			if err := r.reconcile(context.Background()); err != nil {
				t.Fatalf("recovery: %v", err)
			}
			if !d.running || d.engine == nil {
				t.Fatal("replacement is not running")
			}
			if d.creates != 1 {
				t.Fatalf("created %d containers, want exactly one", d.creates)
			}
			if d.engine.Name != "proj-gobackup-1" || d.engine.Image != "huacnlee/gobackup:latest" {
				t.Fatal("engine identity changed")
			}
			if !reflect.DeepEqual(d.engine.Command, []string{"/usr/local/bin/gobackup", "run", "-c", "/etc/gobackup/gobackup.yml"}) {
				t.Fatalf("command changed: %v", d.engine.Command)
			}
			if !reflect.DeepEqual(d.engine.Env, []string{"TZ=Europe/Moscow", "TOKEN=synthetic-secret"}) {
				t.Fatal("engine environment lost")
			}
			if _, ok := d.engine.Networks["backup_net"]; !ok {
				t.Error("engine network lost")
			}
			mounts := map[string]string{}
			for _, m := range d.engine.Mounts {
				mounts[m.Destination] = m.Name
			}
			if !reflect.DeepEqual(mounts, map[string]string{"/etc/gobackup": "config", "/root/.gobackup": "state", "/volumes/app/data": "data"}) {
				t.Errorf("mounts = %v", mounts)
			}
			if err := r.reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if d.creates != 1 || r.Status().LastError != "" {
				t.Error("recovery did not stabilize")
			}
		})
	}
}

func TestReconcile_recoversWithoutCurrentSources(t *testing.T) {
	d := newRecoveryDocker()
	d.failAt = "remove"
	output := filepath.Join(t.TempDir(), "gobackup.yml")
	r := recoveryReconciler(t, d, output)
	if err := r.reconcile(context.Background()); err == nil {
		t.Fatal("expected remove error")
	}
	r = recoveryReconciler(t, d, output)
	r.lister = &fakeLister{}
	if err := r.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !d.running || d.engine == nil {
		t.Fatal("pending replacement was skipped with no current sources")
	}
}

func TestReconcile_recoversBeforeReadingChangedDefaults(t *testing.T) {
	d := newRecoveryDocker()
	d.failAt = "create"
	output := filepath.Join(t.TempDir(), "gobackup.yml")
	r := recoveryReconciler(t, d, output)
	if err := r.reconcile(context.Background()); err == nil {
		t.Fatal("expected create error")
	}
	lastGood, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	r = recoveryReconciler(t, d, output)
	r.cfg.DefaultsPath = writeDefaults(t, "default: [unclosed\n")
	if err := r.reconcile(context.Background()); err == nil {
		t.Fatal("expected defaults error")
	}
	if d.engine == nil || !d.running {
		t.Fatal("new render failure prevented recovery using the last good config")
	}
	current, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(lastGood) {
		t.Fatal("last good config changed")
	}
}

func TestReconcile_concurrentRemovalIsIdempotent(t *testing.T) {
	d := newRecoveryDocker()
	d.failAt = "stop-disappeared"
	r := recoveryReconciler(t, d, filepath.Join(t.TempDir(), "gobackup.yml"))
	if err := r.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !d.running || d.creates != 1 {
		t.Fatal("concurrent removal prevented replacement")
	}
}

func TestReplacement_requiresDurableRecordBeforeStopping(t *testing.T) {
	d := newRecoveryDocker()
	_, _, err := ensureGobackupContainer(context.Background(), d, gbcontainer.Config{},
		engineExtras{EnvVars: []string{"GB_TEST=test"}}, filepath.Join(t.TempDir(), "absent", "record.json"))
	if err == nil {
		t.Fatal("expected record write failure")
	}
	if d.stops != 0 || !d.running {
		t.Fatal("engine stopped without saving recovery state")
	}
}

func TestReconcile_refusesForeignReplacement(t *testing.T) {
	d := newRecoveryDocker()
	d.failAt = "create-response"
	r := recoveryReconciler(t, d, filepath.Join(t.TempDir(), "gobackup.yml"))
	if err := r.reconcile(context.Background()); err == nil {
		t.Fatal("expected create error")
	}
	// A deployment has taken over the name while our operation was pending.
	d.engine.Labels = map[string]string{gobackupComponentLabel: gobackupComponentValue}
	stops := d.stops
	if err := r.reconcile(context.Background()); err == nil || !strings.Contains(err.Error(), "another container") {
		t.Fatalf("foreign replacement should be left alone: %v", err)
	}
	if d.stops != stops || d.running {
		t.Fatal("foreign replacement was changed")
	}
}

func TestReconcile_rejectsInvalidRecoveryRecord(t *testing.T) {
	for _, content := range []string{`{"OldID":"synthetic-secret"`, `{}`} {
		d := newRecoveryDocker()
		output := filepath.Join(t.TempDir(), "gobackup.yml")
		if err := os.WriteFile(output+".container.json", []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		r := recoveryReconciler(t, d, output)
		err := r.reconcile(context.Background())
		if err == nil || strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatalf("unsafe recovery record error: %v", err)
		}
		if d.stops != 0 {
			t.Fatal("invalid record changed engine")
		}
	}
}

func TestReconcile_retriesWithoutDockerEvents(t *testing.T) {
	d := newRecoveryDocker()
	d.failAt = "create"
	r := recoveryReconciler(t, d, filepath.Join(t.TempDir(), "gobackup.yml"))
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	done := make(chan struct{})
	trigger := make(chan struct{}, 1)
	trigger <- struct{}{}
	go func() { defer close(done); r.Run(ctx, trigger) }()
	defer func() { cancel(); <-done }()
	sawError := false
	for {
		select {
		case <-ctx.Done():
			t.Fatal("controller never recovered without another event")
		case <-time.After(10 * time.Millisecond):
			s := r.Status()
			if s.LastError != "" {
				sawError = true
			}
			if sawError && s.LastError == "" {
				cancel()
				<-done
				if !d.running || d.creates != 1 {
					t.Fatal("successful status without running replacement")
				}
				return
			}
		}
	}
}

func TestReconcile_missingEngineWithoutRecoveryState(t *testing.T) {
	d := newRecoveryDocker()
	d.engine = nil
	r := recoveryReconciler(t, d, filepath.Join(t.TempDir(), "gobackup.yml"))
	if err := r.reconcile(context.Background()); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing engine should fail visibly, got %v", err)
	}
	if d.creates != 0 {
		t.Fatal("must not invent missing engine settings")
	}
}

func TestReconcile_recoveryRecordIsPrivate(t *testing.T) {
	d := newRecoveryDocker()
	d.failAt = "remove"
	output := filepath.Join(t.TempDir(), "gobackup.yml")
	r := recoveryReconciler(t, d, output)
	_ = r.reconcile(context.Background())
	info, err := os.Stat(output + ".container.json")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("recovery state permissions = %o", info.Mode().Perm())
	}
	if err := r.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(output + ".container.json"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("completed recovery state not removed: %v", err)
	}
}
