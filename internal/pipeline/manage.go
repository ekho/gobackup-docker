package pipeline

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"

	gbcontainer "github.com/ekho/gobackup-docker/internal/container"
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
	cfg gbcontainer.Config,
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

	// The desired mount set preserves the container's own mounts (config, backups,
	// state — anything not under /volumes/) and adds the discovered archive mounts.
	// Without this the recreated container would lose /etc/gobackup and /backups,
	// and mountsEqual would never stabilize (endless recreate).
	desired := mergeMounts(existing.Mounts, desiredMounts)

	if mountsEqual(existing.Mounts, desired) {
		return existing.ID, false, nil
	}

	log.Printf("[manage] gobackup container %q mounts changed — recreating", existing.Name)

	log.Printf("[manage] stopping container %s ...", shortID(existing.ID))
	if err := mgr.ContainerStop(ctx, existing.ID, nil); err != nil {
		return "", false, fmt.Errorf("stop gobackup %s: %w", shortID(existing.ID), err)
	}

	log.Printf("[manage] removing container %s ...", shortID(existing.ID))
	if err := mgr.ContainerRemove(ctx, existing.ID, false); err != nil {
		return "", false, fmt.Errorf("remove gobackup %s: %w", shortID(existing.ID), err)
	}

	// Build the new container's spec from the supervisor's gobackup_container.*
	// labels (cfg), falling back to the removed container's settings per field.
	spec := buildGobackupSpec(cfg, *existing, desired)

	newID, err := mgr.ContainerCreate(ctx, spec)
	if err != nil {
		return "", false, fmt.Errorf("create gobackup: %w", err)
	}

	log.Printf("[manage] starting new container %s ...", shortID(newID))
	if err := mgr.ContainerStart(ctx, newID); err != nil {
		return "", false, fmt.Errorf("start gobackup %s: %w", shortID(newID), err)
	}

	log.Printf("[manage] gobackup container recreated: %s -> %s", shortID(existing.ID), shortID(newID))
	return newID, true, nil
}

// buildGobackupSpec assembles the recreated gobackup container's spec. The
// supervisor's gobackup_container.* labels (cfg) take precedence per field; any
// field the supervisor did not set falls back to the removed container's own
// settings. The component label is always forced on so the container stays
// discoverable on the next reconcile, and the name is preserved.
func buildGobackupSpec(cfg gbcontainer.Config, existing docker.InspectResult, mounts []docker.MountDef) docker.ContainerSpec {
	spec := docker.ContainerSpec{
		Name:   existing.Name,
		Mounts: mounts,
	}

	spec.Image = existing.Image
	if cfg.Image != "" {
		spec.Image = cfg.Image
	}

	spec.Command = existing.Command
	if cfg.Command != "" {
		spec.Command = strings.Fields(cfg.Command)
	}

	spec.Env = existing.Env
	if len(cfg.Env) > 0 {
		spec.Env = cfg.Env
	}

	spec.Networks = networkKeys(existing.Networks)
	if len(cfg.Networks) > 0 {
		spec.Networks = cfg.Networks
	}

	// Labels: keep the existing container's labels (compose metadata etc.),
	// overlay any gobackup_container.labels.* passthrough, then force the
	// component label so findGobackupContainer can locate it next time.
	labels := make(map[string]string, len(existing.Labels)+len(cfg.Labels)+1)
	for k, v := range existing.Labels {
		labels[k] = v
	}
	for k, v := range cfg.Labels {
		labels[k] = v
	}
	labels[gobackupComponentLabel] = gobackupComponentValue
	spec.Labels = labels

	return spec
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

// archiveMountPrefix is where discovered source volumes are mounted inside the
// gobackup container; mounts under it are considered supervisor-managed.
const archiveMountPrefix = "/volumes/"

// mergeMounts returns the full desired mount set for the gobackup container:
// every existing mount that is NOT a managed archive mount (config, backups,
// state, ...) converted to a MountDef, plus the freshly-discovered archive
// mounts. Stale archive mounts (under /volumes/) are dropped and replaced.
func mergeMounts(existing []container.MountPoint, archive []docker.MountDef) []docker.MountDef {
	out := make([]docker.MountDef, 0, len(existing)+len(archive))
	for _, mp := range existing {
		if strings.HasPrefix(mp.Destination, archiveMountPrefix) {
			continue
		}
		out = append(out, mountPointToDef(mp))
	}
	out = append(out, archive...)
	return out
}

// mountPointToDef converts an inspected MountPoint into the create-time MountDef.
func mountPointToDef(mp container.MountPoint) docker.MountDef {
	src := mp.Source
	if mp.Name != "" {
		src = mp.Name
	}
	return docker.MountDef{
		Type:     mp.Type,
		Source:   src,
		Target:   mp.Destination,
		ReadOnly: !mp.RW,
	}
}

// shortID truncates a container ID to 12 chars for logging, tolerating IDs
// shorter than 12 (real Docker IDs are 64 hex, but be defensive).
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func networkKeys(networks map[string]*network.EndpointSettings) []string {
	keys := make([]string, 0, len(networks))
	for name := range networks {
		keys = append(keys, name)
	}
	return keys
}
