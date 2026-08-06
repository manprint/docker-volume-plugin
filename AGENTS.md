# Docker Volume Plugin

Volume plugin for Docker (`docker plugin`) with volume backup/restore support.

## Repo layout

```
AGENTS.md
Specs.md
Makefile
go.mod
cmd/
  dvol-plugin/       # volume driver daemon (runs inside the plugin rootfs)
  dvol-ctl/          # host-side CLI for backup/restore/list
  dvol-cli-plugin/   # docker CLI plugin ("docker dvol") + compose backup/restore
pkg/
  dvol/              # core library: volume store, backup/restore, compose parsing
plugin/
  config.json        # docker plugin manifest
  Dockerfile         # builds the plugin rootfs
test/
```

## Commands

- `make build`        — build binaries into `bin/`
- `make test`         — run unit tests
- `make e2e`          — CLI smoke test
- `make lint`         — go vet
- `make plugin-build` — build rootfs image
- `make plugin-create`— pack rootfs into a docker plugin
- `make plugin-install` / `make plugin-enable` — install + enable
- `make cli-plugin-install` — install `docker dvol` CLI plugin into ~/.docker/cli-plugins
- `make dev`          — run the driver locally (no docker plugin)

## Key decisions

- Backing store: local host dir (`/var/lib/dvol`), data at `volumes/<name>`.
- Backup format: tar.gz (root-preserved) into `backups/`.
- Docker CLI cannot mount plugins under builtin subcommands (e.g. `docker volume
  backup`), so backup/restore is exposed as the CLI plugin `docker dvol` and
  compose support as `docker dvol compose-backup|-restore`.
- Compose backups pack all project volumes into a single archive
  (`backups/compose/<project>-<ts>.tar.gz`) with a `manifest.json`; each volume
  is a top-level dir in the archive (<project>_<name> naming unless overridden).
- Driver API implemented with `github.com/docker/go-plugins-helpers/volume`.
- Plugin manifest: `interface.docker.volumedriver/1.0`, bind-mounts host
  `/var/lib/dvol` into the plugin at `/data`.
