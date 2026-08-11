package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

var errDockerVolumeNotFound = errors.New("docker volume not found")

const restoreOwnerLabel = "com.github.fabio.dvol.restore-owner"

type dockerVolumeInfo struct {
	Name       string
	Driver     string
	Mountpoint string
	Options    map[string]string
	Labels     map[string]string
}

type dockerVolumeClient interface {
	List() ([]dockerVolumeInfo, error)
	Inspect(name string) (dockerVolumeInfo, error)
	Create(name, restoreOwner string) (dockerVolumeInfo, error)
	Remove(name string) error
	// ContainersUsing returns the names of the running containers that have the
	// volume mounted.
	ContainersUsing(name string) ([]string, error)
}

// dockerCLI uses the Docker CLI instead of linking the full Engine client.
// This keeps the host-side plugin small and automatically respects Docker's
// context, DOCKER_HOST and authentication configuration.
type dockerCLI struct {
	command string
}

func newDockerCLI() dockerVolumeClient { return &dockerCLI{command: "docker"} }

func (c *dockerCLI) run(args ...string) ([]byte, error) {
	out, err := exec.Command(c.command, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (c *dockerCLI) Inspect(name string) (dockerVolumeInfo, error) {
	out, err := c.run("volume", "inspect", "--format", "{{json .}}", name)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "no such volume") || strings.Contains(msg, "not found") {
			return dockerVolumeInfo{}, errDockerVolumeNotFound
		}
		return dockerVolumeInfo{}, err
	}
	var info dockerVolumeInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &info); err != nil {
		return dockerVolumeInfo{}, fmt.Errorf("decode Docker volume %q: %w", name, err)
	}
	if info.Name == "" {
		info.Name = name
	}
	return info, nil
}

func (c *dockerCLI) List() ([]dockerVolumeInfo, error) {
	out, err := c.run("volume", "ls", "--quiet")
	if err != nil {
		return nil, err
	}
	var volumes []dockerVolumeInfo
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		info, err := c.Inspect(name)
		if err != nil {
			if errors.Is(err, errDockerVolumeNotFound) {
				continue // removed between ls and inspect
			}
			return nil, err
		}
		volumes = append(volumes, info)
	}
	return volumes, nil
}

func (c *dockerCLI) Create(name, restoreOwner string) (dockerVolumeInfo, error) {
	if _, err := c.run("volume", "create", "--label", restoreOwnerLabel+"="+restoreOwner, "--name", name); err != nil {
		return dockerVolumeInfo{}, err
	}
	return c.Inspect(name)
}

func (c *dockerCLI) Remove(name string) error {
	if _, err := c.run("volume", "rm", name); err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "no such volume") || strings.Contains(msg, "not found") {
			return nil
		}
		return err
	}
	return nil
}

// ContainersUsing lists the running containers that mount the volume. Restoring
// into a mounted volume replaces the directory a container already holds open:
// the container keeps writing into the detached inode and never sees the
// restored data, so the CLI refuses unless the operator forces it.
func (c *dockerCLI) ContainersUsing(name string) ([]string, error) {
	out, err := c.run("ps", "--filter", "volume="+name, "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}
