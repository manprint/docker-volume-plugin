# Docker Volume Plugin

A Docker volume driver plugin with **volume backup/restore**, usable via
`docker plugin`.

The plugin registers the `docker.volumedriver/1.0` interface. Volumes are
stored in a local backing directory and can be snapshotted to tar.gz archives
and restored from them.

## How it works

```
+--------------------- host ---------------------+
| docker plugin "dvol"                             |
|   /dvol-plugin  (driver daemon, unix socket)     |
|   /data          <- bind mount ->  /var/lib/dvol |
+--------------------------------------------------+
        /var/lib/dvol/
           volumes/<name>/      volume data (shared into containers)
           meta/<name>.json    volume metadata
           backups/<name>/<name>-<ts>.tar.gz
```

- The plugin daemon implements the Docker volume driver protocol over a Unix
  socket (`dvol.sock`) using `github.com/docker/go-plugins-helpers/volume`.
- Host `/var/lib/dvol` is bind-mounted into the plugin at `/data`
  (`propagatedMount`), so container mountpoints resolve to files on the host.
- `dvol-ctl` runs on the host against the same backing dir and performs
  backup/restore without going through Docker.

## Layout

```
cmd/dvol-plugin       volume driver daemon (plugin entrypoint)
cmd/dvol-ctl          host CLI: create/rm/list/backup/restore/backups
cmd/dvol-cli-plugin   docker CLI plugin `docker dvol` (+ compose backup/restore)
pkg/dvol              core library: volume store + tar.gz backup/restore + compose parsing
plugin/               config.json manifest + Dockerfile for the rootfs
test/e2e.sh           end-to-end smoke test over dvol-ctl
```

## Build & test

```
make build   # binaries -> bin/
make test    # unit tests
make lint    # go vet
```

## Package as a docker plugin

Requires `docker`. Data is stored on the host in `/var/lib/dvol`.

```
make plugin-install   # build rootfs + create + install + enable
```

The plugin is named `dvol`. Then create volumes and use them:

```
docker volume create -d dvol --name app
docker run --rm -v app:/data ubuntu sh -c 'echo hi >/data/file'
```

Debug locally without Docker:

```
make dev               # driver on ./dev-dvol
sudo ./bin/dvol-plugin -root /var/lib/dvol   # real socket under /run/docker/plugins
```

## Backup / restore

`dvol-ctl` talks to the backing directory directly. Use it on the host or from
inside a container that mounts `/var/lib/dvol`.

```
dvol-ctl list
dvol-ctl create app
dvol-ctl backup app                # -> backups/app/app-<ts>.tar.gz
dvol-ctl backups app               # list archives
dvol-ctl restore app <name|path>   # restore latest (glob or exact name)
```

### From inside Docker: the `docker dvol` CLI plugin

Install the companion CLI plugin and use the dvol volumes directly from the
`docker` command (requires the `dvol` volume driver plugin, or the store at
`/var/lib/dvol`):

```
make cli-plugin-install     # -> ~/.docker/cli-plugins/docker-dvol
docker dvol list
docker dvol backup app db data1,data2
docker dvol restore app db
docker dvol backups app
```

### Compose-aware backups

One archive for the whole project. The compose file is read to resolve the
project name and its named volumes (`<project>_<name>` unless overridden by
`name:`/`external:`), then all of them are packed into a single file:

```
docker dvol compose-backup                # uses compose.yaml / compose.yml
docker dvol compose-backup -f mycompose.yml
docker dvol compose-backup -f mycompose.yml -p myproj
docker dvol compose-restore -f mycompose.yml
docker dvol compose-restore -f mycompose.yml backups/compose/myproj-<ts>.tar.gz
```

Bundles are stored in `backups/compose/<project>-<ts>.tar.gz`; each volume is a
top-level directory in the archive plus a `manifest.json` listing the contents.
Restore replaces each volume; missing volumes are created.

Scheduled backups are supported by calling the CLI from cron, e.g.:

```cron
0 2 * * *  /usr/local/bin/dvol-ctl backup app web-db
```

## Design notes

- Backup format: root-preserving `tar.gz` (ownership, modes, symlinks kept).
- Restore is "replace": the target volume is wiped, then the archive is
  extracted; a volume is auto-created if it does not already exist.
- Archive member paths are validated at restore time to reject traversal
  (`../`/absolute) attacks.
- Volume names follow Docker's rule `[a-zA-Z0-9][a-zA-Z0-9_.-]*`.