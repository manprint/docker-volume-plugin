# Docker Volume Plugin

Volume plugin for Docker (`docker plugin`) with volume backup/restore support.

## Repo layout

```
AGENTS.md
Makefile
go.mod
cmd/
  dvol-plugin/   # volume driver daemon (runs inside the plugin rootfs)
  dvol-ctl/      # host-side CLI for backup/restore/list
pkg/
  dvol/          # core library: volume store + backup/restore
plugin/
  config.json    # docker plugin manifest
  Dockerfile     # builds the plugin rootfs
test/
```

## Commands

- `make build`        — build binaries into `bin/`
- `make test`         — run unit tests
- `make lint`         — go vet
- `make plugin-build` — build rootfs image
- `make plugin-create`— pack rootfs into a docker plugin
- `make plugin-install` / `make plugin-enable` — install + enable
- `make dev`          — run the driver locally (no docker plugin)

## Key decisions

- Backing store: local host dir (`/var/lib/dvol`), data at `volumes/<name>`.
- Backup format: tar.gz (root-preserved) into `backups/`.
- Driver API implemented with `github.com/docker/go-plugins-helpers/volume`.
- Plugin manifest: `interface.docker.volumedriver/1.0`, bind-mounts host
  `/var/lib/dvol` into the plugin at `/data`.
