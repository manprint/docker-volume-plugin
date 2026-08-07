#!/usr/bin/env bash
# End-to-end smoke test for the dvol-ctl CLI (no docker required).
set -euo pipefail

ROOT="$(mktemp -d)"
D=./bin/dvol-ctl
trap 'rm -rf "$ROOT"' EXIT

echo "== e2e: create volume"
$D -root "$ROOT" create app web

echo "== e2e: list"
$D -root "$ROOT" list

echo "== e2e: write data + backup"
mkdir -p "$ROOT/volumes/app/www"
printf '<html>hello</html>\n' > "$ROOT/volumes/app/www/index.html"
ln -s www "$ROOT/volumes/app/current"
$D -root "$ROOT" backup app

echo "== e2e: corrupt + restore"
rm -rf "$ROOT/volumes/app"
$D -root "$ROOT" restore app 'app-*.tar.gz'
[[ "$(cat "$ROOT/volumes/app/www/index.html")" == '<html>hello</html>' ]]
[[ "$(readlink "$ROOT/volumes/app/current")" == "www" ]]

echo "== e2e: backups list"
$D -root "$ROOT" backups app

echo "== e2e: restore into non-existent volume (auto-create)"
$D -root "$ROOT" restore newvol "$ROOT/backups/app/"app-*.tar.gz

echo "== e2e: gzip level 9 backup + restore"
$D -root "$ROOT" backup app --compression gzip --level 9
rm -rf "$ROOT/volumes/app"
$D -root "$ROOT" restore app "$ROOT"/backups/app/*.tar.gz
[[ "$(cat "$ROOT/volumes/app/www/index.html")" == '<html>hello</html>' ]]

echo "== e2e: xz roundtrip"
$D -root "$ROOT" backup app --compression xz --level 6
rm -rf "$ROOT/volumes/app"
$D -root "$ROOT" restore app "$ROOT"/backups/app/*.tar.xz
[[ "$(cat "$ROOT/volumes/app/www/index.html")" == '<html>hello</html>' ]]
[[ "$(readlink "$ROOT/volumes/app/current")" == "www" ]]

echo "== e2e: zstd roundtrip"
$D -root "$ROOT" backup app --compression zstd
rm -rf "$ROOT/volumes/app"
$D -root "$ROOT" restore app "$ROOT"/backups/app/*.tar.zst
[[ "$(cat "$ROOT/volumes/app/www/index.html")" == '<html>hello</html>' ]]

echo "== e2e: docker CLI plugin metadata"
PLUGIN=./bin/dvol-cli-plugin
$PLUGIN docker-cli-plugin-metadata | grep -q '"SchemaVersion":"0.1.0"'
echo "== e2e: docker dvol list"
$PLUGIN dvol list --root "$ROOT"
echo "== e2e: docker dvol backup (comma-separated)"
mkdir -p "$ROOT/volumes/web/www"
printf '<html>cli</html>\n' > "$ROOT/volumes/web/www/index.html"
$PLUGIN dvol backup web,newvol --root "$ROOT"
echo "== e2e: docker dvol restore"
rm -rf "$ROOT/volumes/web"
$PLUGIN dvol restore web --root "$ROOT"
[[ "$(cat "$ROOT/volumes/web/www/index.html")" == '<html>cli</html>' ]]

echo "== e2e: docker dvol --backup-dir custom"
CUST="$ROOT/custom-backups"
mkdir -p "$CUST"
$PLUGIN dvol backup web,newvol --root "$ROOT" --backup-dir "$CUST"
ls "$CUST"/web/*.tar.gz >/dev/null
rm -rf "$ROOT/volumes/web"
$PLUGIN dvol restore web --root "$ROOT" --backup-dir "$CUST"
[[ "$(cat "$ROOT/volumes/web/www/index.html")" == '<html>cli</html>' ]]

echo "== e2e: compose bundle (single archive, all volumes)"
COMPOSE_DIR="$ROOT/compose-src"
mkdir -p "$COMPOSE_DIR"
cat > "$COMPOSE_DIR/compose.yaml" <<YAML
name: e2eproj
volumes:
  data:
  cache:
YAML
$D -root "$ROOT" create e2eproj_data e2eproj_cache
printf 'value=1\n' > "$ROOT/volumes/e2eproj_data/settings.ini"
$PLUGIN dvol compose-backup -f "$COMPOSE_DIR/compose.yaml" --root "$ROOT" | grep -q "backed up 2 volume"
# Keep these store-managed volumes in place: this smoke test intentionally does
# not require a Docker daemon. A real Docker-volume restore is exercised by the
# host CLI against the mountpoint returned by Docker.
$PLUGIN dvol compose-restore -f "$COMPOSE_DIR/compose.yaml" --root "$ROOT" | grep -q "restored 2 volume"
[[ "$(cat "$ROOT/volumes/e2eproj_data/settings.ini")" == 'value=1' ]]
BUNDLES=("$ROOT"/backups/compose/*.tar.gz)
[[ ${#BUNDLES[@]} -eq 1 ]]

echo "e2e OK"
