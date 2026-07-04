package pipeline

import (
	"context"
	"fmt"
	"log"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"

	"github.com/ekho/gobackup-docker/internal/docker"
)

// ContainerManager groups all Docker operations the reconciler needs for
// archive volume support.
type ContainerManager interface {
	ContainerInspect(ctx context.Context, id string) (docker.InspectResult, error)
	ContainerCreate(ctx context.Context, spec docker.ContainerSpec) (string, error)
	ContainerStart(ctx context.Context, id string) error
	ContainerStop(ctx context.Context, id string, timeout *int) error
	ContainerRemove(ctx context.Context, id string, force bool) error
	ListAll(ctx context.Context) ([]docker.Container, error)
}

const gobackupComponentLabel = "gobackup-docker.component"
const gobackupComponentValue = "gobackup"

func findGobackupContainer(ctx context.Context, mgr ContainerManager) (*docker.InspectResult, error) {
	containers, err := mgr.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range containers {
		if c.Labels[gobackupComponentLabel] == gobackupComponentValue {
			info, err := mgr.ContainerInspect(ctx, c.ID)
			if err != nil {
				return nil, err
			}
			return &info, nil
		}
	}
	return nil, nil
}

func ensureGobackupContainerMounts(
	ctx context.Context,
	mgr ContainerManager,
	desiredMounts []docker.MountDef,
) (string, bool, error) {
	existing, err := findGobackupContainer(ctx, mgr)
	if err != nil {
		return "", false, fmt.Errorf("find gobackup container: %w", err)
	}
	if existing == nil {
		return "", false, fmt.Errorf("gobackup container not found (label %s=%s missing)",
			gobackupComponentLabel, gobackupComponentValue)
	}

	if mountsEqual(existing.Mounts, desiredMounts) {
		return existing.ID, false, nil
	}

	log.Printf("[manage] gobackup container %q mounts changed — recreating", existing.Name)

	log.Printf("[manage] stopping container %s ...", existing.ID[:12])
	if err := mgr.ContainerStop(ctx, existing.ID, nil); err != nil {
		return "", false, fmt.Errorf("stop gobackup %s: %w", existing.ID[:12], err)
	}

	log.Printf("[manage] removing container %s ...", existing.ID[:12])
	if err := mgr.ContainerRemove(ctx, existing.ID, false); err != nil {
		return "", false, fmt.Errorf("remove gobackup %s: %w", existing.ID[:12], err)
	}

	spec := docker.ContainerSpec{
		Image:    existing.Image,
		Command:  existing.Command,
		Env:      existing.Env,
		Labels:   existing.Labels,
		Networks: networkKeys(existing.Networks),
		Mounts:   desiredMounts,
	}

	newID, err := mgr.ContainerCreate(ctx, spec)
	if err != nil {
		return "", false, fmt.Errorf("create gobackup: %w", err)
	}

	log.Printf("[manage] starting new container %s ...", newID[:12])
	if err := mgr.ContainerStart(ctx, newID); err != nil {
		return "", false, fmt.Errorf("start gobackup %s: %w", newID[:12], err)
	}

	log.Printf("[manage] gobackup container recreated: %s -> %s", existing.ID[:12], newID[:12])
	return newID, true, nil
}

func mountsEqual(current []container.MountPoint, desired []docker.MountDef) bool {
	if len(current) != len(desired) {
		return false
	}
	want := make(map[string]bool, len(desired))
	for _, d := range desired {
		want[mountKey(d.Type, d.Source, d.Target, d.ReadOnly)] = true
	}
	for _, c := range current {
		src := c.Source
		if c.Name != "" {
			src = c.Name
		}
		if !want[mountKey(c.Type, src, c.Destination, !c.RW)] {
			return false
		}
	}
	return true
}

func mountKey(mType mount.Type, source, target string, readOnly bool) string {
	return fmt.Sprintf("%s|%s|%s|%t", string(mType), source, target, readOnly)
}

func networkKeys(networks map[string]*network.EndpointSettings) []string {
	keys := make([]string, 0, len(networks))
	for name := range networks {
		keys = append(keys, name)
	}
	return keys
}
