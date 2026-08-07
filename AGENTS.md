# Docker Volume Plugin

Volume plugin for Docker (`docker plugin`) with volume backup/restore support.

## Repo layout

```
AGENTS.md
Specs.md
Makefile
go.mod
cmd/
  dvol-cli-plugin/   # docker CLI plugin ("docker dvol") + compose backup/restore
pkg/
  dvol/              # core library: archive backup/restore and compose parsing
test/
```

## Commands

- `make build`        — build the Docker CLI plugin into `bin/`
- `make test`         — run unit tests
- `make e2e`          — Docker volume discovery smoke test
- `make lint`         — go vet
- `make cli-plugin-install` — install `docker dvol` CLI plugin into ~/.docker/cli-plugins

## Key decisions

- Archive repository: local host directory, configurable with `--root` and
  `--backup-dir`; Docker volume data stays managed by Docker.
- Backup format: tar.gz (root-preserved) into `backups/`.
- Docker CLI cannot mount plugins under builtin subcommands (e.g. `docker volume
  backup`), so backup/restore is exposed as the CLI plugin `docker dvol` and
  compose support as `docker dvol compose-backup|-restore`.
- Compose backups pack all project volumes into a single archive
  (`backups/compose/<project>-<ts>.tar.gz`) with a `manifest.json`; each volume
  is a top-level dir in the archive (<project>_<name> naming unless overridden).
- Docker volumes are discovered through the Docker CLI and operated on through
  their real mountpoints.
