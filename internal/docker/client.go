// Package docker wraps the Docker Engine SDK for the two things the supervisor
// needs: listing containers (with their gobackup.* labels) and streaming
// container start/die events. After label-driven config generation it also
// manages the gobackup container lifecycle (inspect, create, stop, remove).
package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

// Client is a thin wrapper over the Docker SDK client.
type Client struct {
	cli *client.Client
}

// Container is the minimal view of a running container the supervisor cares about.
type Container struct {
	ID     string
	Name   string // primary name, de-slashed (e.g. "nextcloud", not "/nextcloud")
	Labels map[string]string
}

type InspectResult struct {
	ID       string
	Name     string
	Image    string
	Command  []string
	Env      []string
	Labels   map[string]string
	Mounts   []container.MountPoint
	Networks map[string]*network.EndpointSettings
}

// ContainerSpec describes a container to create. Fields map directly to the
// Docker SDK's container.Config, HostConfig, and NetworkingConfig.
type ContainerSpec struct {
	Image    string
	Command  []string
	Env      []string
	Labels   map[string]string
	Name     string // desired container name
	Mounts   []MountDef
	Networks []string
}

// MountDef describes a single mount in the HostConfig.Mounts format.
type MountDef struct {
	Type        mount.Type // TypeVolume, TypeBind, or TypeTmpfs
	Source      string     // volume name or host path
	Target      string     // container path
	ReadOnly    bool
	BindOptions *mount.BindOptions
}

// New creates a Docker client from the environment (DOCKER_HOST, TLS, etc.),
// negotiating the API version with the daemon. This honours rootless / remote
// Docker without hardcoding /var/run/docker.sock.
func New(ctx context.Context) (*Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	if _, err := cli.Ping(ctx); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("ping docker daemon: %w", err)
	}
	return &Client{cli: cli}, nil
}

// Close releases the underlying HTTP client.
func (c *Client) Close() error { return c.cli.Close() }

// HostID returns the Docker daemon's hostname (system.Info.Name), used as the
// default suffix in auto-generated model names to disambiguate backups from
// different hosts writing to the same storage. Overridable via env upstream.
func (c *Client) HostID(ctx context.Context) (string, error) {
	info, err := c.cli.Info(ctx)
	if err != nil {
		return "", fmt.Errorf("docker info: %w", err)
	}
	return info.Name, nil
}

// List returns the currently running containers. Gating on gobackup.enable is
// done later (in package labels) so the exposedByDefault policy lives in one place.
func (c *Client) List(ctx context.Context) ([]Container, error) {
	summaries, err := c.cli.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(),
	})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	out := make([]Container, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, Container{
			ID:     s.ID,
			Name:   primaryName(s.Names),
			Labels: s.Labels,
		})
	}
	return out, nil
}

// ListAll returns all containers (including stopped), used to find a
// previously-managed gobackup container that may be stopped.
func (c *Client) ListAll(ctx context.Context) ([]Container, error) {
	summaries, err := c.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(),
	})
	if err != nil {
		return nil, fmt.Errorf("list all containers: %w", err)
	}
	out := make([]Container, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, Container{
			ID:     s.ID,
			Name:   primaryName(s.Names),
			Labels: s.Labels,
		})
	}
	return out, nil
}

// ContainerInspect returns a supervisor-friendly view of a container's mounts,
// labels, image, command, env, and networks. Used for volume discovery and
// supervisor self-inspection.
func (c *Client) ContainerInspect(ctx context.Context, id string) (InspectResult, error) {
	raw, err := c.cli.ContainerInspect(ctx, id)
	if err != nil {
		return InspectResult{}, fmt.Errorf("inspect container %s: %w", id[:min(len(id), 12)], err)
	}

	result := InspectResult{
		ID:     raw.ID,
		Name:   strings.TrimPrefix(raw.Name, "/"),
		Mounts: raw.Mounts,
	}

	if raw.Config != nil {
		result.Image = raw.Config.Image
		result.Command = raw.Config.Cmd
		result.Env = raw.Config.Env
		result.Labels = raw.Config.Labels
	}

	if raw.NetworkSettings != nil {
		result.Networks = raw.NetworkSettings.Networks
	}

	return result, nil
}

// ---------------------------------------------------------------------------
// Container lifecycle
// ---------------------------------------------------------------------------

// ContainerCreate creates a container from the given spec and returns its ID.
func (c *Client) ContainerCreate(ctx context.Context, spec ContainerSpec) (string, error) {
	env := spec.Env
	if env == nil {
		env = []string{}
	}

	cfg := &container.Config{
		Image:  spec.Image,
		Cmd:    spec.Command,
		Env:    env,
		Labels: spec.Labels,
	}

	hostMounts := make([]mount.Mount, 0, len(spec.Mounts))
	for _, m := range spec.Mounts {
		hostMounts = append(hostMounts, mount.Mount{
			Type:        m.Type,
			Source:      m.Source,
			Target:      m.Target,
			ReadOnly:    m.ReadOnly,
			BindOptions: m.BindOptions,
		})
	}

	hostCfg := &container.HostConfig{
		Mounts: hostMounts,
	}

	var netCfg *network.NetworkingConfig
	if len(spec.Networks) > 0 {
		netCfg = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{},
		}
		for _, n := range spec.Networks {
			netCfg.EndpointsConfig[n] = &network.EndpointSettings{}
		}
	}

	resp, err := c.cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, spec.Name)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	return resp.ID, nil
}

// ContainerStart starts an existing container by ID.
func (c *Client) ContainerStart(ctx context.Context, id string) error {
	if err := c.cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return fmt.Errorf("start container %s: %w", id[:min(len(id), 12)], err)
	}
	return nil
}

// ContainerStop stops a container gracefully with the given timeout (seconds).
// A nil timeout uses the container's or daemon's default.
func (c *Client) ContainerStop(ctx context.Context, id string, timeout *int) error {
	opts := container.StopOptions{}
	if timeout != nil {
		opts.Timeout = timeout
	}
	if err := c.cli.ContainerStop(ctx, id, opts); err != nil {
		return fmt.Errorf("stop container %s: %w", id[:min(len(id), 12)], err)
	}
	return nil
}

// ContainerRemove removes a stopped container. Use Force: true to kill and
// remove a running container.
func (c *Client) ContainerRemove(ctx context.Context, id string, force bool) error {
	opts := container.RemoveOptions{Force: force}
	if err := c.cli.ContainerRemove(ctx, id, opts); err != nil {
		return fmt.Errorf("remove container %s: %w", id[:min(len(id), 12)], err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func primaryName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
