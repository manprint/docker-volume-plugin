#!/usr/bin/env bash
set -Eeuo pipefail

# Contract acceptance for binary stdin/stdout. dvol is invoked through the
# Docker CLI plugin; rclone runs in its official Docker image and uses a
# bind-mounted directory as a local filesystem remote.

SCRIPT_DIR="$(cd -- "$(dirname -- "$0")" && pwd)"
PROJECT_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
WORK_DIR="$(mktemp -d /tmp/docker-volume-stream-acceptance.XXXXXX)"
REMOTE_ROOT="$WORK_DIR/remote"
COMPOSE_FILE="$WORK_DIR/stream-compose.yaml"
PROJECT="dvolstream_$$"
mkdir -p "$REMOTE_ROOT"
VOLUMES=""
COMPOSE_PROJECTS=""
NEW_VOLUME=""

if [[ "$(id -u)" -eq 0 ]]; then
  DVOL_PREFIX=(docker dvol)
else
  DVOL_PREFIX=(docker run --rm -i --user 0
    -v /var/run/docker.sock:/var/run/docker.sock
    -v /var/lib/docker:/var/lib/docker
    -v "$HOME/.docker/cli-plugins:/root/.docker/cli-plugins:ro"
    -v "$WORK_DIR:$WORK_DIR:ro"
    -v "$PROJECT_ROOT:/workspace:ro" -w /workspace docker:cli dvol)
fi
dvol_cmd() { "${DVOL_PREFIX[@]}" "$@"; }

fail() { echo "STREAM ACCEPTANCE FAILURE: $*" >&2; return 1; }
cleanup() {
  local volume project
  set +e
  for volume in $VOLUMES; do docker volume rm -f "$volume" >/dev/null 2>&1; done
  for project in $COMPOSE_PROJECTS; do docker compose -p "$project" -f "$COMPOSE_FILE" down -v --remove-orphans >/dev/null 2>&1; done
  if [[ -d "$REMOTE_ROOT" ]]; then docker run --rm -i -v "$REMOTE_ROOT:/remote" rclone/rclone:latest purge /remote >/dev/null 2>&1; fi
  rm -f "$COMPOSE_FILE"
  rmdir "$REMOTE_ROOT" >/dev/null 2>&1 || true
  rmdir "$WORK_DIR" >/dev/null 2>&1 || true
}
trap cleanup EXIT

require_image() { local image="$1"; docker image inspect "$image" >/dev/null 2>&1 || docker pull "$image" >/dev/null; }
rclone_cmd() { docker run --rm -i -v "$REMOTE_ROOT:/remote" rclone/rclone:latest "$@"; }
new_volume() {
  local name="dvol_stream_$PROJECT-$1"
  docker volume create "$name" >/dev/null
  VOLUMES="$VOLUMES $name"
  NEW_VOLUME="$name"
}
populate_volume() {
  local volume="$1" marker="$2"
  docker run --rm -v "$volume:/data" alpine:3.20 sh -ec "mkdir -p /data/nested; printf '%s\n' '$marker' > /data/data.txt; printf '%s\n' 'nested-$marker' > /data/nested/value.txt; ln -sf data.txt /data/link.txt"
}
assert_volume() {
  local volume="$1" marker="$2"
  docker run --rm -v "$volume:/data" alpine:3.20 sh -ec "test \"\$(cat /data/data.txt)\" = '$marker'; test \"\$(cat /data/nested/value.txt)\" = 'nested-$marker'; test -L /data/link.txt; test \"\$(readlink /data/link.txt)\" = data.txt"
}
volume_state() {
  local volume="$1"
  docker run --rm -v "$volume:/data:ro" alpine:3.20 sh -ec '
    find /data -xdev -print | sort | while IFS= read -r path; do
      stat -c "%n|%F|%a|%u|%g|%s|%Y" "$path"
      if [ -f "$path" ] && [ ! -L "$path" ]; then sha256sum "$path"; fi
    done
  '
}
volume_fingerprint() {
  volume_state "$1" | sha256sum | awk '{print $1}'
}
assert_volume_unchanged() {
  local volume="$1" before="$2" after
  after="$(volume_fingerprint "$volume")"
  if [[ "$before" != "$after" ]]; then
    echo "Source state after backup ($volume):" >&2
    volume_state "$volume" >&2
    fail "source volume changed during backup: $volume ($before -> $after)"
  fi
}
assert_remote_object() {
  local object="$1" size
  size="$(rclone_cmd size --json "$object" | tr -d '\n' | sed -n 's/.*"bytes"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p')"
  [[ "$size" =~ ^[0-9]+$ && "$size" -gt 0 ]] || fail "remote object is missing or empty: $object"
  rclone_cmd cat "$object" | sha256sum | grep -Eq '^[0-9a-f]{64}  -$' || fail "remote object cannot be read: $object"
}
compression_ext() {
  case "$1" in
    gzip) printf 'gz\n' ;;
    xz) printf 'xz\n' ;;
    zstd) printf 'zst\n' ;;
    lz4) printf 'lz4\n' ;;
    *) fail "unknown compression $1" ;;
  esac
}

run_single_matrix() {
  local compression="$1" ext volume object marker status before mountpoint forbidden
  ext="$(compression_ext "$compression")"
  new_volume "single-$compression"
  volume="$NEW_VOLUME"
  marker="single-$compression-$PROJECT"
  populate_volume "$volume" "$marker"
  object="/remote/single/$volume.tar.$ext"
  echo "stream single backup/restore compression=$compression"
  before="$(volume_fingerprint "$volume")"
  dvol_cmd backup --stdout --compression "$compression" --threads 1 "$volume" | rclone_cmd rcat "$object"
  assert_volume_unchanged "$volume" "$before"
  assert_remote_object "$object"
  if [[ "$compression" == gzip ]]; then
    mountpoint="$(docker volume inspect -f '{{.Mountpoint}}' "$volume")"
    forbidden="$mountpoint/.dvol-forbidden-output"
    before="$(volume_fingerprint "$volume")"
    set +e
    dvol_cmd backup --destination "$forbidden" --archive-name forbidden "$volume" >/dev/null 2>&1
    status=$?
    set -e
    (( status != 0 )) || fail "backup destination inside source was accepted"
    assert_volume_unchanged "$volume" "$before"
  fi
  docker run --rm -v "$volume:/data" alpine:3.20 sh -ec "printf '%s\n' changed > /data/data.txt; touch /data/temporary.txt"
  rclone_cmd cat "$object" | dvol_cmd restore --stdin "$volume"
  assert_volume "$volume" "$marker"
  docker run --rm -v "$volume:/data" alpine:3.20 test ! -e /data/temporary.txt
  docker run --rm -v "$volume:/data" alpine:3.20 sh -ec "printf '%s\n' changed-again > /data/data.txt; touch /data/temporary.txt"
  rclone_cmd cat "$object" | dvol_cmd restore --stdin --spool "$volume"
  assert_volume "$volume" "$marker"
  docker run --rm -v "$volume:/data" alpine:3.20 test ! -e /data/temporary.txt
  docker run --rm -v "$volume:/data" alpine:3.20 sh -ec "printf '%s\n' preserved > /data/data.txt"
  set +e
  # Cut inside the compressed payload (rather than only its optional frame
  # trailer, which some decoders can treat as an otherwise complete tar).
  rclone_cmd cat "$object" | head -c 64 | dvol_cmd restore --stdin "$volume" >/dev/null 2>&1
  status=$?
  set -e
  (( status != 0 )) || fail "truncated stream unexpectedly restored: $compression"
  docker run --rm -v "$volume:/data" alpine:3.20 sh -ec 'test "$(cat /data/data.txt)" = preserved'
}

run_bundle_matrix() {
  local compression="$1" ext v1 v2 object before1 before2
  ext="$(compression_ext "$compression")"
  new_volume "bundle-$compression-data"
  v1="$NEW_VOLUME"
  new_volume "bundle-$compression-logs"
  v2="$NEW_VOLUME"
  populate_volume "$v1" "bundle-data-$compression"
  populate_volume "$v2" "bundle-logs-$compression"
  object="/remote/bundle/$PROJECT-$compression.tar.$ext"
  echo "stream bundle backup/restore compression=$compression"
  before1="$(volume_fingerprint "$v1")"
  before2="$(volume_fingerprint "$v2")"
  dvol_cmd backup --stdout --bundle --compression "$compression" --threads 1 "$v1" "$v2" | rclone_cmd rcat "$object"
  assert_volume_unchanged "$v1" "$before1"
  assert_volume_unchanged "$v2" "$before2"
  assert_remote_object "$object"
  docker run --rm -v "$v1:/data" alpine:3.20 sh -ec 'printf changed > /data/data.txt; touch /data/temporary.txt'
  docker run --rm -v "$v2:/data" alpine:3.20 sh -ec 'printf changed > /data/data.txt; touch /data/temporary.txt'
  rclone_cmd cat "$object" | dvol_cmd restore --stdin --bundle "$v1" "$v2"
  assert_volume "$v1" "bundle-data-$compression"
  assert_volume "$v2" "bundle-logs-$compression"
  docker run --rm -v "$v1:/data" alpine:3.20 test ! -e /data/temporary.txt
  docker run --rm -v "$v2:/data" alpine:3.20 test ! -e /data/temporary.txt
}

run_compose_matrix() {
  local compression="$1" ext object before_data before_logs
  ext="$(compression_ext "$compression")"
  cat > "$COMPOSE_FILE" <<'YAML'
services:
  app:
    image: alpine:3.20
    command: ["sh", "-c", "while :; do sleep 3600; done"]
    volumes:
      - data:/data
      - logs:/logs
volumes:
  data:
  logs:
YAML
  docker compose -p "$PROJECT" -f "$COMPOSE_FILE" up -d >/dev/null
  COMPOSE_PROJECTS="$COMPOSE_PROJECTS $PROJECT"
  docker compose -p "$PROJECT" -f "$COMPOSE_FILE" exec -T app sh -ec "printf '%s\n' compose-$compression > /data/data.txt; printf '%s\n' log-$compression > /logs/log.txt"
  object="/remote/compose/$PROJECT-$compression.tar.$ext"
  echo "stream compose backup/restore compression=$compression"
  before_data="$(volume_fingerprint "${PROJECT}_data")"
  before_logs="$(volume_fingerprint "${PROJECT}_logs")"
  dvol_cmd compose-backup --stdout -f "$COMPOSE_FILE" -p "$PROJECT" --compression "$compression" --threads 1 | rclone_cmd rcat "$object"
  assert_volume_unchanged "${PROJECT}_data" "$before_data"
  assert_volume_unchanged "${PROJECT}_logs" "$before_logs"
  assert_remote_object "$object"
  docker compose -p "$PROJECT" -f "$COMPOSE_FILE" down -v >/dev/null
  rclone_cmd cat "$object" | dvol_cmd compose-restore --stdin -f "$COMPOSE_FILE" -p "$PROJECT"
  docker compose -p "$PROJECT" -f "$COMPOSE_FILE" up -d >/dev/null
  docker compose -p "$PROJECT" -f "$COMPOSE_FILE" exec -T app sh -ec "test \"\$(cat /data/data.txt)\" = compose-$compression; test \"\$(cat /logs/log.txt)\" = log-$compression"
  docker compose -p "$PROJECT" -f "$COMPOSE_FILE" exec -T app sh -ec 'printf changed > /data/data.txt'
  # Restore while the service is quiesced; replacing a mounted directory while
  # a container is running cannot provide a coherent application view.
  docker compose -p "$PROJECT" -f "$COMPOSE_FILE" down >/dev/null
  rclone_cmd cat "$object" | dvol_cmd compose-restore --stdin --spool -f "$COMPOSE_FILE" -p "$PROJECT"
  docker compose -p "$PROJECT" -f "$COMPOSE_FILE" up -d >/dev/null
  docker compose -p "$PROJECT" -f "$COMPOSE_FILE" exec -T app sh -ec "test \"\$(cat /data/data.txt)\" = compose-$compression"
  docker compose -p "$PROJECT" -f "$COMPOSE_FILE" down -v >/dev/null
}

require_image rclone/rclone:latest
require_image alpine:3.20
for compression in gzip xz zstd lz4; do
  run_single_matrix "$compression"
  run_bundle_matrix "$compression"
done
for compression in gzip xz zstd lz4; do run_compose_matrix "$compression"; done

echo "Stream acceptance passed: Docker dvol + rclone remote, single/bundle/Compose, all compressions, direct/spooled restore and truncation checks."
