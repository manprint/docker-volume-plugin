package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fakeDockerCommand(t *testing.T) (*dockerCLI, string) {
	t.Helper()
	dir := t.TempDir()
	command := filepath.Join(dir, "docker")
	logPath := filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
printf 'CALL' >> "$DVOL_FAKE_DOCKER_LOG"
for arg in "$@"; do printf '\t%s' "$arg" >> "$DVOL_FAKE_DOCKER_LOG"; done
printf '\n' >> "$DVOL_FAKE_DOCKER_LOG"

case "$1 $2" in
  "volume ls")
    printf 'alpha\ngone\n'
    ;;
  "volume inspect")
    case "$5" in
      alpha) printf '{"Name":"alpha","Driver":"local","Mountpoint":"/var/lib/docker/volumes/alpha/_data","Options":{}}\n' ;;
      created) printf '{"Name":"created","Driver":"local","Mountpoint":"/var/lib/docker/volumes/created/_data","Options":{},"Labels":{"com.github.fabio.dvol.restore-owner":"test-owner"}}\n' ;;
      unnamed) printf '{"Driver":"local","Mountpoint":"/tmp/unnamed","Options":{}}\n' ;;
      malformed) printf '{not-json}\n' ;;
      *) printf 'Error: No such volume: %s\n' "$5" >&2; exit 1 ;;
    esac
    ;;
  "volume create")
    printf '%s\n' "$6"
    ;;
  "volume rm")
    case "$3" in
      missing) printf 'Error: No such volume: missing\n' >&2; exit 1 ;;
      busy) printf 'volume is in use\n' >&2; exit 1 ;;
      *) printf '%s\n' "$3" ;;
    esac
    ;;
  "ps --filter")
    printf 'app-1\napp-2\n'
    ;;
  *)
    printf 'unexpected fake Docker arguments\n' >&2
    exit 2
    ;;
esac
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DVOL_FAKE_DOCKER_LOG", logPath)
	return &dockerCLI{command: command}, logPath
}

func TestDockerCLIAdapterCommandsAndParsing(t *testing.T) {
	cli, logPath := fakeDockerCommand(t)

	info, err := cli.Inspect("unnamed")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "unnamed" || info.Driver != "local" || info.Mountpoint != "/tmp/unnamed" {
		t.Fatalf("unexpected inspect result: %+v", info)
	}

	volumes, err := cli.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(volumes) != 1 || volumes[0].Name != "alpha" {
		t.Fatalf("list did not tolerate inspect/remove race: %+v", volumes)
	}

	created, err := cli.Create("created", "test-owner")
	if err != nil || created.Name != "created" {
		t.Fatalf("create: info=%+v err=%v", created, err)
	}
	if err := cli.Remove("created"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := cli.Remove("missing"); err != nil {
		t.Fatalf("remove must be idempotent for a missing volume: %v", err)
	}
	if err := cli.Remove("busy"); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("non-not-found remove failure was hidden: %v", err)
	}

	containers, err := cli.ContainersUsing("alpha")
	if err != nil || !reflect.DeepEqual(containers, []string{"app-1", "app-2"}) {
		t.Fatalf("containers using volume: names=%v err=%v", containers, err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logData)
	for _, want := range []string{
		"CALL\tvolume\tinspect\t--format\t{{json .}}\tunnamed",
		"CALL\tvolume\tls\t--quiet",
		"CALL\tvolume\tcreate\t--label\tcom.github.fabio.dvol.restore-owner=test-owner\t--name\tcreated",
		"CALL\tvolume\trm\tcreated",
		"CALL\tps\t--filter\tvolume=alpha\t--format\t{{.Names}}",
	} {
		if !strings.Contains(logText, want) {
			t.Errorf("Docker invocation %q missing from log:\n%s", want, logText)
		}
	}
}

func TestDockerCLIAdapterClassifiesFailures(t *testing.T) {
	cli, _ := fakeDockerCommand(t)
	if _, err := cli.Inspect("gone"); !errors.Is(err, errDockerVolumeNotFound) {
		t.Fatalf("not-found classification: %v", err)
	}
	if _, err := cli.Inspect("malformed"); err == nil || !strings.Contains(err.Error(), "decode Docker volume") {
		t.Fatalf("invalid inspect JSON was accepted: %v", err)
	}
	if got := newDockerCLI(); got == nil {
		t.Fatal("newDockerCLI returned nil")
	}
}
