#!/usr/bin/env sh
set -eu

# End-to-end smoke test for the Docker CLI plugin. Data operations require
# permission to access Docker's mountpoint, so this smoke test covers the
# Docker discovery path and leaves the data path to unit tests.

if ! command -v docker >/dev/null 2>&1; then
	echo "SKIP: Docker CLI is not installed"
	exit 0
fi

PLUGIN=${PLUGIN:-./bin/dvol-cli-plugin}
ROOT=$(mktemp -d)
trap 'rm -rf "$ROOT"' EXIT

"$PLUGIN" --root "$ROOT" list >/dev/null
"$PLUGIN" --root "$ROOT" help >/dev/null 2>&1
echo "e2e smoke test passed"
