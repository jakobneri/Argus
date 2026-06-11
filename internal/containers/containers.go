// Package containers provides read-only access to Docker containers.
// External systems sit behind the Manager interface so they can be mocked
// in tests.
package containers

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// Container describes one Docker container.
type Container struct {
	Name   string
	Image  string
	State  string
	Status string
}

// Manager lists Docker containers. Implementations must be read-only.
// An unreachable Docker daemon is reported as an error; callers treat it as
// non-fatal (empty list plus a hint), never as a crash.
type Manager interface {
	List(ctx context.Context) ([]Container, error)
}

// containerLister is the slice of the Docker SDK client that the manager
// needs; it exists so tests can substitute a fake backend.
type containerLister interface {
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	Close() error
}

// DockerManager implements Manager against the Docker Engine API via the
// official SDK (pure Go). The connection uses the default environment
// (DOCKER_HOST or /var/run/docker.sock).
type DockerManager struct {
	connect func() (containerLister, error)
}

// NewDockerManager returns a Manager backed by the local Docker daemon.
func NewDockerManager() *DockerManager {
	return &DockerManager{
		connect: func() (containerLister, error) {
			cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
			if err != nil {
				return nil, fmt.Errorf("create docker client: %w", err)
			}
			return cli, nil
		},
	}
}

// List returns all containers (running and stopped), sorted by name.
func (m *DockerManager) List(ctx context.Context) ([]Container, error) {
	cli, err := m.connect()
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	defer func() { _ = cli.Close() }()

	summaries, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("docker: list containers: %w", err)
	}
	return containersFromSummaries(summaries), nil
}

// containersFromSummaries converts SDK summaries to Containers, sorted by name.
func containersFromSummaries(summaries []container.Summary) []Container {
	out := make([]Container, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, Container{
			Name:   primaryName(s.Names),
			Image:  s.Image,
			State:  s.State,
			Status: s.Status,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// primaryName returns the first container name without the leading slash the
// Docker API prefixes onto names.
func primaryName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}
