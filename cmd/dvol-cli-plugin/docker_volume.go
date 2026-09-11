package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

var (
	errDockerVolumeNotFound    = errors.New("docker volume not found")
	errDockerContainerNotFound = errors.New("docker container not found")
)

const (
	restoreOwnerLabel      = "com.github.fabio.dvol.restore-owner"
	volumeLockLabel        = "com.github.fabio.dvol.locked-volume"
	volumeLockManagedLabel = "com.github.fabio.dvol.volume-lock"
	volumeLockVersion      = "1"
	volumeLockMount        = "/dvol-lock"
	defaultVolumeLockImage = "busybox:1.36.1"
)

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
	// LockVolume creates or restarts a Docker-managed guard reference. The
	// returned boolean reports whether a new guard container was created.
	LockVolume(name, image string) (bool, error)
	// UnlockVolume removes dvol's guard reference. The returned boolean reports
	// whether a guard existed.
	UnlockVolume(name string) (bool, error)
	// ContainersUsing returns the names of the running containers that have the
	// volume mounted, excluding dvol's own lock guard.
	ContainersUsing(name string) ([]string, error)
}

type dockerContainerInfo struct {
	ID     string
	Name   string
	Config struct {
		Labels map[string]string
	}
	State struct {
		Running bool
	}
	Mounts []struct {
		Type        string
		Name        string
		Destination string
		RW          bool
	}
}

// dockerCLI uses the Docker CLI instead of linking the full Engine client.
// This keeps the host-side plugin small and automatically respects Docker's
// context, DOCKER_HOST and authentication configuration.
type dockerCLI struct {
	command string
}

func newDockerCLI() dockerVolumeClient { return &dockerCLI{command: "docker"} }

// run executes the Docker CLI and returns its standard output only.
//
// Merging the two streams was wrong in both directions. The Docker CLI writes
// advisory lines to stderr on a perfectly healthy daemon ("WARNING: ..." about
// API version negotiation, deprecated configuration, credential helpers), and
// every one of them landed in the middle of the JSON handed to the decoder or
// in the volume list handed to `Inspect`: a plain `docker dvol list` failed
// with `invalid character 'W' looking for beginning of value`. Diagnostics
// still reach the operator, through the error message of a failed command.
func (c *dockerCLI) run(args ...string) ([]byte, error) {
	cmd := exec.Command(c.command, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		details := strings.TrimSpace(stderr.String())
		if details == "" {
			details = strings.TrimSpace(stdout.String())
		}
		return stdout.Bytes(), fmt.Errorf("%w: %s", err, details)
	}
	return stdout.Bytes(), nil
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

func volumeLockContainerName(volume string) string {
	sum := sha256.Sum256([]byte(volume))
	return fmt.Sprintf("dvol-lock-%x", sum[:12])
}

func (c *dockerCLI) inspectContainer(name string) (dockerContainerInfo, error) {
	out, err := c.run("container", "inspect", "--format", "{{json .}}", name)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "no such container") || strings.Contains(msg, "not found") {
			return dockerContainerInfo{}, errDockerContainerNotFound
		}
		return dockerContainerInfo{}, err
	}
	var info dockerContainerInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &info); err != nil {
		return dockerContainerInfo{}, fmt.Errorf("decode Docker container %q: %w", name, err)
	}
	return info, nil
}

func isVolumeLockContainer(info dockerContainerInfo, volume string) bool {
	if info.Config.Labels[volumeLockManagedLabel] != volumeLockVersion ||
		info.Config.Labels[volumeLockLabel] != volume {
		return false
	}
	for _, mount := range info.Mounts {
		if mount.Type == "volume" && mount.Name == volume &&
			mount.Destination == volumeLockMount && !mount.RW {
			return true
		}
	}
	return false
}

func (c *dockerCLI) LockVolume(name, image string) (bool, error) {
	guard := volumeLockContainerName(name)
	info, err := c.inspectContainer(guard)
	if err == nil {
		if !isVolumeLockContainer(info, name) {
			return false, fmt.Errorf("docker container %q collides with the dvol lock name but is not the lock for volume %q", guard, name)
		}
		if !info.State.Running {
			if _, err := c.run("container", "start", guard); err != nil {
				return false, fmt.Errorf("restart lock guard %q: %w; its stopped reference still protects volume %q from direct removal", guard, err, name)
			}
		}
		return false, nil
	}
	if !errors.Is(err, errDockerContainerNotFound) {
		return false, err
	}
	if image == "" {
		image = defaultVolumeLockImage
	}
	_, createErr := c.run("container", "create",
		"--name", guard,
		"--label", volumeLockManagedLabel+"="+volumeLockVersion,
		"--label", volumeLockLabel+"="+name,
		"--network", "none",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges=true",
		"--restart", "unless-stopped",
		"--mount", "type=volume,source="+name+",target="+volumeLockMount+",readonly",
		image, "tail", "-f", "/dev/null")
	if createErr != nil {
		// Docker container names are unique. If another lock operation won the
		// race, accept its correctly labelled guard instead of failing.
		info, inspectErr := c.inspectContainer(guard)
		if inspectErr != nil || !isVolumeLockContainer(info, name) {
			return false, fmt.Errorf("create lock guard %q: %w", guard, createErr)
		}
		if info.State.Running {
			return false, nil
		}
		if _, err := c.run("container", "start", guard); err != nil {
			return false, fmt.Errorf("restart concurrently created lock guard %q: %w", guard, err)
		}
		return false, nil
	}
	if _, err := c.run("container", "start", guard); err != nil {
		return true, fmt.Errorf("start lock guard %q: %w; its stopped reference still protects volume %q from direct removal", guard, err, name)
	}
	return true, nil
}

func (c *dockerCLI) UnlockVolume(name string) (bool, error) {
	guard := volumeLockContainerName(name)
	info, err := c.inspectContainer(guard)
	if errors.Is(err, errDockerContainerNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !isVolumeLockContainer(info, name) {
		return false, fmt.Errorf("docker container %q collides with the dvol lock name but is not the lock for volume %q", guard, name)
	}
	if _, err := c.run("container", "rm", "--force", guard); err != nil {
		return false, fmt.Errorf("remove lock guard %q: %w", guard, err)
	}
	return true, nil
}

// ContainersUsing lists the running containers that mount the volume. Restoring
// into a mounted volume replaces the directory a container already holds open:
// the container keeps writing into the detached inode and never sees the
// restored data, so the CLI refuses unless the operator forces it.
func (c *dockerCLI) ContainersUsing(name string) ([]string, error) {
	out, err := c.run("ps", "--filter", "volume="+name, "--format", "{{.ID}}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id := strings.TrimSpace(line)
		if id == "" {
			continue
		}
		info, err := c.inspectContainer(id)
		if errors.Is(err, errDockerContainerNotFound) {
			continue // removed between ps and inspect
		}
		if err != nil {
			return nil, err
		}
		if isVolumeLockContainer(info, name) {
			continue
		}
		containerName := strings.TrimPrefix(info.Name, "/")
		if containerName == "" {
			containerName = id
		}
		names = append(names, containerName)
	}
	return names, nil
}
