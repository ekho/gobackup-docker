package pipeline

import (
	"context"
	"encoding/json"
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

// baseCmdLabel records the UNWRAPPED engine command as JSON on the recreated
// container, so a subsequent reconcile recovers the base rather than re-wrapping
// an already-wrapped command.
const baseCmdLabel = "gobackup-docker.base-command"

// engineExtras are the supervisor-computed additions to the recreated gobackup
// container: managed mounts (archive volumes + secret files), credential env
// vars (from _env credentials), and secret command-exports (from _file creds).
type engineExtras struct {
	ExtraMounts   []docker.MountDef
	EnvVars       []string
	SecretExports []secretExport
}

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

func ensureGobackupContainer(
	ctx context.Context,
	mgr ContainerManager,
	cfg gbcontainer.Config,
	extras engineExtras,
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
	// state — anything not managed) and adds the archive + secret mounts. Without
	// this the recreated container would lose /etc/gobackup and /backups, and the
	// recreate check would never stabilize (endless recreate).
	desiredMounts := mergeMounts(existing.Mounts, extras.ExtraMounts)
	spec := buildGobackupSpec(cfg, *existing, desiredMounts, extras.EnvVars, extras.SecretExports)

	if !needsRecreate(*existing, spec, extras.EnvVars) {
		return existing.ID, false, nil
	}

	log.Printf("[manage] gobackup container %q spec changed — recreating", existing.Name)

	log.Printf("[manage] stopping container %s ...", shortID(existing.ID))
	if err := mgr.ContainerStop(ctx, existing.ID, nil); err != nil {
		return "", false, fmt.Errorf("stop gobackup %s: %w", shortID(existing.ID), err)
	}

	log.Printf("[manage] removing container %s ...", shortID(existing.ID))
	if err := mgr.ContainerRemove(ctx, existing.ID, false); err != nil {
		return "", false, fmt.Errorf("remove gobackup %s: %w", shortID(existing.ID), err)
	}

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

// secretExport binds an env var to a secret file path inside the engine
// container, materialized at container start by the command wrapper.
type secretExport struct {
	Var  string
	Path string
}

// wrapCommandWithSecrets wraps the engine's base command so that each secret
// file is read into an env var at start, then `exec`s the base command.
// mechanism (B): the secret value never touches the config volume or docker
// inspect (only the PATH appears in Cmd); `exec` makes gobackup PID 1 so
// `docker stop` signals it. With no exports the base command is returned as-is.
func wrapCommandWithSecrets(base []string, exports []secretExport) []string {
	if len(exports) == 0 {
		return base
	}
	var sb strings.Builder
	for _, e := range exports {
		// value flows through cat's stdout into a double-quoted $()—never
		// re-tokenized; the path is single-quote-escaped so a hostile label
		// path cannot break out.
		fmt.Fprintf(&sb, `export %s="$(cat %s)"; `, e.Var, shQuote(e.Path))
	}
	sb.WriteString("exec")
	for _, a := range base {
		sb.WriteString(" ")
		sb.WriteString(shQuote(a))
	}
	return []string{"/bin/sh", "-c", sb.String()}
}

// shQuote wraps s in POSIX single quotes, escaping embedded single quotes as
// '\” — making any string safe to embed in a /bin/sh -c command.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildGobackupSpec assembles the recreated gobackup container's spec. The
// supervisor's gobackup_container.* labels (cfg) take precedence per field; any
// field the supervisor did not set falls back to the removed container's own
// settings. The component label is always forced on so the container stays
// discoverable on the next reconcile, and the name is preserved.
func buildGobackupSpec(cfg gbcontainer.Config, existing docker.InspectResult, mounts []docker.MountDef, envVars []string, secretExports []secretExport) docker.ContainerSpec {
	spec := docker.ContainerSpec{
		Name:   existing.Name,
		Mounts: mounts,
	}

	spec.Image = existing.Image
	if cfg.Image != "" {
		spec.Image = cfg.Image
	}

	// Command: recover the UNWRAPPED base first (never re-wrap an already-wrapped
	// existing.Command), then wrap it to source any _file secrets at start.
	base := baseCommand(cfg, existing)
	spec.Command = wrapCommandWithSecrets(base, secretExports)

	// Env: base (gobackup_container.env or existing) + credential vars (win).
	baseEnv := existing.Env
	if len(cfg.Env) > 0 {
		baseEnv = cfg.Env
	}
	spec.Env = mergeEnv(baseEnv, envVars)

	spec.Networks = networkKeys(existing.Networks)
	if len(cfg.Networks) > 0 {
		spec.Networks = cfg.Networks
	}

	// Labels: keep the existing container's labels (compose metadata etc.),
	// overlay gobackup_container.labels.* passthrough, force the component label
	// (so findGobackupContainer locates it next time), and record the UNWRAPPED
	// base command so the next reconcile recovers it instead of re-wrapping.
	labels := make(map[string]string, len(existing.Labels)+len(cfg.Labels)+2)
	for k, v := range existing.Labels {
		labels[k] = v
	}
	for k, v := range cfg.Labels {
		labels[k] = v
	}
	labels[gobackupComponentLabel] = gobackupComponentValue
	labels[baseCmdLabel] = encodeBaseCmd(base)
	spec.Labels = labels

	return spec
}

// baseCommand resolves the UNWRAPPED engine command: an explicit
// gobackup_container.command wins; otherwise the base recorded on the existing
// container (so a wrapped command is never re-wrapped); otherwise existing as-is.
func baseCommand(cfg gbcontainer.Config, existing docker.InspectResult) []string {
	if cfg.Command != "" {
		return strings.Fields(cfg.Command)
	}
	if b, ok := decodeBaseCmd(existing.Labels[baseCmdLabel]); ok {
		return b
	}
	return existing.Command
}

func encodeBaseCmd(cmd []string) string {
	b, _ := json.Marshal(cmd)
	return string(b)
}

func decodeBaseCmd(s string) ([]string, bool) {
	if s == "" {
		return nil, false
	}
	var cmd []string
	if err := json.Unmarshal([]byte(s), &cmd); err != nil || len(cmd) == 0 {
		return nil, false
	}
	return cmd, true
}

// mergeEnv appends credential vars to the base env, with credential vars winning
// over a base entry of the same key.
func mergeEnv(base, creds []string) []string {
	if len(creds) == 0 {
		return base
	}
	credKey := make(map[string]bool, len(creds))
	for _, c := range creds {
		credKey[envKey(c)] = true
	}
	out := make([]string, 0, len(base)+len(creds))
	for _, e := range base {
		if !credKey[envKey(e)] {
			out = append(out, e)
		}
	}
	return append(out, creds...)
}

func envKey(e string) string {
	if i := strings.IndexByte(e, '='); i >= 0 {
		return e[:i]
	}
	return e
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

// needsRecreate decides whether the engine must be recreated to reach the desired
// spec. It checks the managed dimensions only — mounts, the (possibly wrapped)
// command, and the presence of each credential env var — deliberately ignoring
// image-injected env like PATH so the check stabilizes instead of looping.
func needsRecreate(existing docker.InspectResult, spec docker.ContainerSpec, credVars []string) bool {
	if !mountsEqual(existing.Mounts, spec.Mounts) {
		return true
	}
	if !slicesEqual(existing.Command, spec.Command) {
		return true
	}
	for _, cv := range credVars {
		if !containsString(existing.Env, cv) {
			return true // credential var missing or its value changed (rotation)
		}
	}
	return false
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Supervisor-managed mount prefixes: archive volumes and secret files live under
// these. Everything else on the engine (config, /backups, state) is preserved
// across recreates.
const (
	archiveMountPrefix = "/volumes/"
	secretMountPrefix  = "/gobackup-secrets/"
)

func isManagedMount(dest string) bool {
	return strings.HasPrefix(dest, archiveMountPrefix) || strings.HasPrefix(dest, secretMountPrefix)
}

// mergeMounts returns the full desired mount set for the gobackup container:
// every existing mount that is NOT supervisor-managed (config, backups, state,
// ...) converted to a MountDef, plus the freshly-computed managed mounts. Stale
// managed mounts (archive volumes, secret files) are dropped and replaced.
func mergeMounts(existing []container.MountPoint, managed []docker.MountDef) []docker.MountDef {
	out := make([]docker.MountDef, 0, len(existing)+len(managed))
	for _, mp := range existing {
		if isManagedMount(mp.Destination) {
			continue
		}
		out = append(out, mountPointToDef(mp))
	}
	out = append(out, managed...)
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
