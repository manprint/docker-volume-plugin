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

echo "e2e OK"