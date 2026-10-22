//go:build integration

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/errdefs"

	gbcontainer "github.com/ekho/gobackup-docker/internal/container"
	"github.com/ekho/gobackup-docker/internal/docker"
)

type removalRaceDocker struct {
	*docker.Client
	name    string
	removed bool
}

func (d *removalRaceDocker) ListAll(ctx context.Context) ([]docker.Container, error) {
	info, err := d.ContainerInspect(ctx, d.name)
	if errdefs.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []docker.Container{{ID: info.ID, Name: info.Name, Labels: info.Labels}}, nil
}

func (d *removalRaceDocker) ContainerRemove(ctx context.Context, id string, force bool) error {
	if err := d.Client.ContainerRemove(ctx, id, force); err != nil {
		return err
	}
	if !d.removed {
		d.removed = true
		return errdefs.Conflict(errors.New("removal of container is already in progress"))
	}
	return nil
}

// Run with a local Docker daemon and alpine:3.23 already pulled:
// go test -tags=integration ./internal/pipeline -run TestDocker_recovery -v
func TestDocker_recovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client, err := docker.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	name := fmt.Sprintf("gobackup-recovery-test-%d", time.Now().UnixNano())
	d := &removalRaceDocker{Client: client, name: name}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if err := client.ContainerRemove(cleanup, name, true); err != nil && !errdefs.IsNotFound(err) {
			t.Error(err)
		}
	}()
	base := docker.ContainerSpec{
		Name: name, Image: "alpine:3.23", Command: []string{"sleep", "300"}, Env: []string{"TOKEN=synthetic-secret"},
		Labels: map[string]string{gobackupComponentLabel: gobackupComponentValue},
		Mounts: []docker.MountDef{{Type: mount.TypeBind, Source: t.TempDir(), Target: "/etc/gobackup"}},
	}
	id, err := d.ContainerCreate(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ContainerStart(ctx, id); err != nil {
		t.Fatal(err)
	}
	extras := engineExtras{ExtraMounts: []docker.MountDef{{Type: mount.TypeBind, Source: t.TempDir(), Target: "/volumes/app/data", ReadOnly: true}}}
	record := filepath.Join(t.TempDir(), "gobackup.yml.container.json")
	if _, _, err := ensureGobackupContainer(ctx, d, gbcontainer.Config{}, extras, record); !errdefs.IsConflict(err) {
		t.Fatalf("expected injected removal race, got %v", err)
	}
	newID, recreated, err := ensureGobackupContainer(ctx, d, gbcontainer.Config{}, extras, record)
	if err != nil {
		t.Fatal(err)
	}
	if !recreated || newID == id {
		t.Fatal("engine was not replaced")
	}
	info, err := client.ContainerInspect(ctx, newID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != name || !containsString(info.Env, "TOKEN=synthetic-secret") || len(info.Mounts) != 2 {
		t.Fatal("replacement lost settings")
	}
	running, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Running}}", newID).Output()
	if err != nil || strings.TrimSpace(string(running)) != "true" {
		t.Fatalf("engine not running: %s %v", running, err)
	}
	if _, recreated, err := ensureGobackupContainer(ctx, d, gbcontainer.Config{}, extras, record); err != nil || recreated {
		t.Fatalf("replacement did not stabilize: recreated=%v err=%v", recreated, err)
	}
}
