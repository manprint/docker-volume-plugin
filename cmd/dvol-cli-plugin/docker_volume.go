package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

var errDockerVolumeNotFound = errors.New("docker volume not found")

type dockerVolumeInfo struct {
	Name       string
	Driver     string
	Mountpoint string
}

type dockerVolumeClient interface {
	List() ([]dockerVolumeInfo, error)
	Inspect(name string) (dockerVolumeInfo, error)
	Create(name string) (dockerVolumeInfo, error)
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
			return nil, err
		}
		volumes = append(volumes, info)
	}
	return volumes, nil
}

func (c *dockerCLI) Create(name string) (dockerVolumeInfo, error) {
	if _, err := c.run("volume", "create", "--name", name); err != nil {
		return dockerVolumeInfo{}, err
	}
	return c.Inspect(name)
}
