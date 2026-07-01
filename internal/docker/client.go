// Package docker wraps the Docker Engine SDK for the two things the supervisor
// needs: listing containers (with their gobackup.* labels) and streaming
// container start/die events.
package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
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
		// Running only: a DB dump needs the target up, and start/die events
		// keep the set current. All:true would also surface stopped containers.
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

func primaryName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}
