#!/usr/bin/env bash
set -Eeuo pipefail

# Acceptance suite for the defects fixed after the 2026-08-08 review (REVIEW.md).
# Every scenario reproduces the original failure against real Docker volumes and
# asserts the corrected behaviour. Run as root: dvol reads and replaces Docker
# mountpoints directly.

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/docker-volume-defects.XXXXXX")"
readonly SCRIPT_DIR PROJECT_ROOT WORK_DIR
readonly ARCHIVE_ROOT="$WORK_DIR/archives"
readonly DVOL_BIN="${DVOL_BIN:-$PROJECT_ROOT/bin/dvol-cli-plugin}"
readonly DVOL_MODE="${DVOL_MODE:-direct}"
readonly TAG="dvoldef$$"

dvol_cmd() {
	case "$DVOL_MODE" in
		direct) "$DVOL_BIN" "$@" ;;
		docker) docker dvol "$@" ;;
		*) fail "unsupported DVOL_MODE: $DVOL_MODE" ;;
	esac
}

declare -a CREATED_VOLUMES=()
declare -a CREATED_CONTAINERS=()

cleanup() {
	local container volume
	set +e
	for container in "${CREATED_CONTAINERS[@]}"; do
		docker rm -f "$container" >/dev/null 2>&1
	done
	for volume in "${CREATED_VOLUMES[@]}"; do
		docker volume rm -f "$volume" >/dev/null 2>&1
	done
	rm -rf -- "$WORK_DIR"
}
trap cleanup EXIT

fail() { echo "DEFECT ACCEPTANCE FAILURE: $*" >&2; return 1; }
pass() { echo "  ok: $*"; }

new_volume() {
	local name="dvol_${TAG}_$1"
	docker volume create "$name" >/dev/null
	CREATED_VOLUMES+=("$name")
	printf '%s\n' "$name"
}

mountpoint_of() { docker volume inspect --format '{{.Mountpoint}}' "$1"; }

# in_volume runs a shell snippet inside the volume, as root, with /data mounted.
in_volume() {
	local volume="$1"; shift
	docker run --rm --user 0 -v "$volume:/data" alpine:3.20 sh -ec "$*"
}

# tree_manifest captures everything a restore must reproduce, including
# nanosecond mtimes and extended attributes.
tree_manifest() {
	local mountpoint="$1"
	find "$mountpoint" -mindepth 1 -printf '%P\0' | sort -z |
	while IFS= read -r -d '' rel; do
		local path="$mountpoint/$rel" meta digest attrs
		meta="$(stat -c '%F|%a|%u|%g|%s|%.9Y' -- "$path")"
		attrs="$(getfattr -d -m - --absolute-names -- "$path" 2>/dev/null | tail -n +2 | sort | tr '\n' ';')"
		if [[ -L "$path" ]]; then
			digest="link:$(readlink -- "$path")"
			meta="$(stat -c '%F|%u|%g|%.9Y' -- "$path")"
		elif [[ -f "$path" ]]; then
			digest="file:$(sha256sum -- "$path" | awk '{print $1}')"
		elif [[ -p "$path" ]]; then
			digest="fifo"
		else
			digest="special"
		fi
		printf '%s\t%s\t%s\t%s\n' "$rel" "$meta" "$digest" "$attrs"
	done
}

manifest_digest() { tree_manifest "$1" | sha256sum | awk '{print $1}'; }

expect_failure() {
	local description="$1"; shift
	local status=0
	set +e
	"$@" >"$WORK_DIR/out.log" 2>&1
	status=$?
	set -e
	if (( status == 0 )); then
		echo "--- command output ---" >&2
		cat "$WORK_DIR/out.log" >&2
		fail "$description: command unexpectedly succeeded"
	fi
	pass "$description (refused as expected)"
}

expect_output() {
	local needle="$1"
	grep -Fq -- "$needle" "$WORK_DIR/out.log" || {
		echo "--- command output ---" >&2
		cat "$WORK_DIR/out.log" >&2
		fail "expected output to contain: $needle"
	}
}

# ---------------------------------------------------------------------------
# B1: a bundle restored through the single-volume path destroyed the volume.
# ---------------------------------------------------------------------------
scenario_bundle_into_single_volume() {
	echo "B1: bundle archive refused by the single-volume restore"
	local a b victim bundle before
	a="$(new_volume b1a)"; b="$(new_volume b1b)"; victim="$(new_volume b1victim)"
	in_volume "$a" "printf a > /data/a.txt"
	in_volume "$b" "printf b > /data/b.txt"
	in_volume "$victim" "printf precious > /data/keep.txt"
	before="$(manifest_digest "$(mountpoint_of "$victim")")"

	bundle="$(dvol_cmd --root "$ARCHIVE_ROOT" backup --bundle "$a" "$b" | sed -n 's/^backed up .* -> //p')"
	[[ -f "$bundle" ]] || fail "bundle was not produced"

	expect_failure "restoring a bundle as a single volume" \
		dvol_cmd --root "$ARCHIVE_ROOT" restore "$victim" "$bundle"
	expect_output "multi-volume dvol bundle"

	[[ "$before" == "$(manifest_digest "$(mountpoint_of "$victim")")" ]] \
		|| fail "the refused bundle restore modified the volume"
	pass "victim volume untouched"

	# and the inverse direction
	local single
	single="$(dvol_cmd --root "$ARCHIVE_ROOT" backup "$victim" | sed -n 's/^backed up .* -> //p')"
	expect_failure "restoring a single-volume archive as a bundle" \
		dvol_cmd --root "$ARCHIVE_ROOT" restore --bundle "$victim" "$single"
}

# ---------------------------------------------------------------------------
# B2: a file changing size during the read aborted the whole backup.
# ---------------------------------------------------------------------------
scenario_hot_backup() {
	echo "B2: backup of a volume under continuous writes"
	local volume container archive
	volume="$(new_volume b2)"
	container="dvol-def-writer-$TAG"
	in_volume "$volume" "dd if=/dev/zero of=/data/wal.log bs=1M count=32 2>/dev/null; printf stable > /data/stable.txt"
	docker run -d --name "$container" --user 0 -v "$volume:/data" alpine:3.20 \
		sh -c 'while :; do dd if=/dev/urandom bs=64k count=16 >> /data/wal.log 2>/dev/null; : > /data/rotate.log; dd if=/dev/zero of=/data/rotate.log bs=1M count=8 2>/dev/null; done' >/dev/null
	CREATED_CONTAINERS+=("$container")
	sleep 2

	archive="$(dvol_cmd --root "$ARCHIVE_ROOT" backup --compression lz4 "$volume" | sed -n 's/^backed up .* -> //p')"
	[[ -f "$archive" ]] || fail "hot backup produced no archive"
	pass "hot backup completed"

	dvol_cmd --root "$ARCHIVE_ROOT" verify --archive "$archive" >/dev/null \
		|| fail "archive produced under concurrent writes does not verify"
	pass "archive verifies"

	docker rm -f "$container" >/dev/null
	local restored
	restored="$(new_volume b2restored)"
	dvol_cmd --root "$ARCHIVE_ROOT" restore "$restored" "$archive" >/dev/null
	in_volume "$restored" 'test "$(cat /data/stable.txt)" = stable'
	pass "hot archive restores and keeps untouched files intact"
}

# ---------------------------------------------------------------------------
# B3 + B4: FIFOs and unix sockets inside a volume.
# ---------------------------------------------------------------------------
scenario_special_files() {
	echo "B3/B4: FIFO and unix socket inside the volume"
	local volume archive restored
	volume="$(new_volume b34)"
	in_volume "$volume" "mkfifo -m 640 /data/run.fifo; printf payload > /data/data.txt"
	# a bound unix socket, created by a short-lived python process
	docker run --rm --user 0 -v "$volume:/data" python:3.12-alpine \
		python -c "import socket; s=socket.socket(socket.AF_UNIX); s.bind('/data/app.sock')" >/dev/null

	in_volume "$volume" "test -S /data/app.sock" || fail "unix socket fixture missing"

	archive="$(dvol_cmd --root "$ARCHIVE_ROOT" backup "$volume" | sed -n 's/^backed up .* -> //p')"
	pass "backup of a volume with a FIFO and a socket succeeded"

	restored="$(new_volume b34restored)"
	dvol_cmd --root "$ARCHIVE_ROOT" restore "$restored" "$archive" >/dev/null
	in_volume "$restored" 'test -p /data/run.fifo' || fail "FIFO was not restored"
	in_volume "$restored" 'test "$(stat -c %a /data/run.fifo)" = 640' || fail "FIFO permissions lost"
	in_volume "$restored" 'test "$(cat /data/data.txt)" = payload' || fail "data lost"
	in_volume "$restored" 'test ! -e /data/app.sock' || fail "socket should not be recreated"
	pass "FIFO restored, socket skipped, data intact"
}

# ---------------------------------------------------------------------------
# H1: restoring several volumes when one of them no longer exists.
# ---------------------------------------------------------------------------
scenario_restore_missing_volume() {
	echo "H1: restore of several volumes after one was deleted"
	local web db
	web="$(new_volume h1web)"; db="$(new_volume h1db)"
	in_volume "$web" "printf web-data > /data/data.txt"
	in_volume "$db" "printf db-data > /data/data.txt"
	dvol_cmd --root "$ARCHIVE_ROOT" backup "$web" "$db" >/dev/null

	docker volume rm -f "$db" >/dev/null
	dvol_cmd --root "$ARCHIVE_ROOT" restore "$web" "$db" >/dev/null
	in_volume "$web" 'test "$(cat /data/data.txt)" = web-data' || fail "web not restored"
	in_volume "$db" 'test "$(cat /data/data.txt)" = db-data' || fail "deleted volume not recreated and restored"
	pass "both volumes restored, missing one recreated"
}

# ---------------------------------------------------------------------------
# H2: compose-restore could not find --timestamp bundles.
# ---------------------------------------------------------------------------
scenario_compose_timestamp_lookup() {
	echo "H2: compose-restore finds a timestamped bundle"
	local dir project
	project="${TAG}h2"
	dir="$WORK_DIR/$project"
	mkdir -p "$dir"
	cat > "$dir/compose.yaml" <<YAML
name: $project
services:
  app:
    image: alpine:3.20
    command: ["sh", "-c", "sleep 3600"]
    volumes:
      - data:/data
volumes:
  data:
YAML
	docker volume create "${project}_data" >/dev/null
	CREATED_VOLUMES+=("${project}_data")
	in_volume "${project}_data" "printf compose-payload > /data/data.txt"

	dvol_cmd --root "$ARCHIVE_ROOT" compose-backup -f "$dir/compose.yaml" --timestamp >/dev/null
	in_volume "${project}_data" "printf changed > /data/data.txt"
	dvol_cmd --root "$ARCHIVE_ROOT" compose-restore -f "$dir/compose.yaml" >/dev/null
	in_volume "${project}_data" 'test "$(cat /data/data.txt)" = compose-payload' \
		|| fail "timestamped bundle was not found by compose-restore"
	pass "timestamped bundle located and restored"

	# a bundle renamed with --archive-name is found through its manifest
	dvol_cmd --root "$ARCHIVE_ROOT" compose-backup -f "$dir/compose.yaml" --archive-name nightly >/dev/null
	rm -f "$ARCHIVE_ROOT"/backups/compose/"${project}"_*.tar.gz
	in_volume "${project}_data" "printf changed-again > /data/data.txt"
	dvol_cmd --root "$ARCHIVE_ROOT" compose-restore -f "$dir/compose.yaml" >/dev/null
	in_volume "${project}_data" 'test "$(cat /data/data.txt)" = compose-payload' \
		|| fail "renamed bundle was not found through its manifest"
	pass "renamed bundle located through its manifest"
}

# ---------------------------------------------------------------------------
# H3: compose backup/restore volume-set mismatch and its side effects.
# ---------------------------------------------------------------------------
scenario_compose_volume_set() {
	echo "H3: compose volume set mismatches"
	local dir project
	project="${TAG}h3"
	dir="$WORK_DIR/$project"
	mkdir -p "$dir"
	cat > "$dir/compose.yaml" <<YAML
name: $project
services:
  app:
    image: alpine:3.20
    command: ["sh", "-c", "sleep 3600"]
    volumes:
      - data:/data
      - cache:/cache
volumes:
  data:
  cache:
YAML
	docker volume create "${project}_data" >/dev/null
	CREATED_VOLUMES+=("${project}_data")
	CREATED_VOLUMES+=("${project}_cache")
	in_volume "${project}_data" "printf data-payload > /data/data.txt"

	expect_failure "compose-backup with a declared volume missing from Docker" \
		dvol_cmd --root "$ARCHIVE_ROOT" compose-backup -f "$dir/compose.yaml"
	expect_output "${project}_cache"
	expect_output "--skip-missing"
	! docker volume inspect "${project}_cache" >/dev/null 2>&1 \
		|| fail "the refused compose-backup created a volume"

	local bundle
	bundle="$(dvol_cmd --root "$ARCHIVE_ROOT" compose-backup -f "$dir/compose.yaml" --skip-missing \
		| sed -n 's/^backed up .* -> //p')"
	[[ -f "$bundle" ]] || fail "--skip-missing did not produce a bundle"
	pass "--skip-missing produced a partial bundle"

	expect_failure "compose-restore --strict against a partial bundle" \
		dvol_cmd --root "$ARCHIVE_ROOT" compose-restore -f "$dir/compose.yaml" --strict "$bundle"
	expect_output "${project}_cache"
	! docker volume inspect "${project}_cache" >/dev/null 2>&1 \
		|| fail "a refused compose-restore created the missing volume"
	pass "the refused restore created no volume"

	in_volume "${project}_data" "printf changed > /data/data.txt"
	dvol_cmd --root "$ARCHIVE_ROOT" compose-restore -f "$dir/compose.yaml" "$bundle" 2>"$WORK_DIR/out.log" >/dev/null
	expect_output "${project}_cache"
	in_volume "${project}_data" 'test "$(cat /data/data.txt)" = data-payload' \
		|| fail "the tolerant restore did not restore the volume present in the bundle"
	pass "tolerant restore warns and restores what the bundle carries"
}

# ---------------------------------------------------------------------------
# H5: restoring into a volume a running container has mounted.
# ---------------------------------------------------------------------------
scenario_volume_in_use() {
	echo "H5: restore into a volume mounted by a running container"
	local volume container archive
	volume="$(new_volume h5)"
	container="dvol-def-holder-$TAG"
	in_volume "$volume" "printf archived > /data/data.txt"
	archive="$(dvol_cmd --root "$ARCHIVE_ROOT" backup "$volume" | sed -n 's/^backed up .* -> //p')"
	in_volume "$volume" "printf live > /data/data.txt"

	docker run -d --name "$container" --user 0 -v "$volume:/data" alpine:3.20 sleep 3600 >/dev/null
	CREATED_CONTAINERS+=("$container")

	expect_failure "restore into a mounted volume" \
		dvol_cmd --root "$ARCHIVE_ROOT" restore "$volume" "$archive"
	expect_output "$container"
	expect_output "--force"
	in_volume "$volume" 'test "$(cat /data/data.txt)" = live' \
		|| fail "the refused restore modified the volume"
	pass "refused while the container runs, volume untouched"

	dvol_cmd --root "$ARCHIVE_ROOT" restore --force "$volume" "$archive" 2>"$WORK_DIR/out.log" >/dev/null
	expect_output "mounted by running containers"
	pass "--force overrides the guard and warns"
	docker rm -f "$container" >/dev/null
}

# ---------------------------------------------------------------------------
# H6: an interrupted restore must leave the volume recoverable.
# ---------------------------------------------------------------------------
scenario_interrupted_restore() {
	echo "H6: restore killed mid-flight is repaired on the next run"
	local volume archive mountpoint status
	volume="$(new_volume h6)"
	in_volume "$volume" "printf original > /data/marker.txt; dd if=/dev/urandom of=/data/payload.bin bs=1M count=192 2>/dev/null"
	archive="$(dvol_cmd --root "$ARCHIVE_ROOT" backup --compression lz4 "$volume" | sed -n 's/^backed up .* -> //p')"
	in_volume "$volume" "printf live > /data/marker.txt"
	mountpoint="$(mountpoint_of "$volume")"

	set +e
	timeout -s KILL 1 "$DVOL_BIN" --root "$ARCHIVE_ROOT" restore --force "$volume" "$archive" >/dev/null 2>&1
	status=$?
	set -e
	echo "  interrupted restore exited with $status"

	# Whatever the moment of the kill, the volume must still have a data
	# directory holding one of the two coherent states.
	dvol_cmd --root "$ARCHIVE_ROOT" backups "$volume" >/dev/null
	[[ -d "$mountpoint" ]] || fail "the volume data directory disappeared after the interrupted restore"
	local marker
	marker="$(cat "$mountpoint/marker.txt" 2>/dev/null || true)"
	[[ "$marker" == "live" || "$marker" == "original" ]] \
		|| fail "volume left in an incoherent state after an interrupted restore (marker=$marker)"
	pass "volume coherent after recovery (marker=$marker)"

	# No staging or rollback debris may survive the recovery.
	local debris
	debris="$(find "$(dirname "$mountpoint")" -maxdepth 1 \( -name '.*restore*' -o -name '.*dvol-old*' \) | wc -l)"
	[[ "$debris" == 0 ]] || fail "recovery left $debris staging/rollback directories behind"
	pass "no staging or rollback debris left"

	# And a normal restore still works afterwards.
	dvol_cmd --root "$ARCHIVE_ROOT" restore --force "$volume" "$archive" >/dev/null
	in_volume "$volume" 'test "$(cat /data/marker.txt)" = original' || fail "restore after recovery failed"
	pass "restore works again after recovery"
}

# ---------------------------------------------------------------------------
# Fidelity: metadata, xattrs, sparse files and sub-second timestamps.
# ---------------------------------------------------------------------------
scenario_fidelity() {
	echo "Fidelity: full metadata round trip on a real Docker volume"
	local volume restored archive before after
	volume="$(new_volume fid)"
	docker run --rm --user 0 -v "$volume:/data" alpine:3.20 sh -ec '
		mkdir -p /data/config /data/empty-dir
		printf hello > /data/index.html
		printf secret > /data/config/db.ini
		chmod 600 /data/config/db.ini
		chmod 750 /data/config
		ln -s index.html /data/current
		ln /data/index.html /data/index.hard
		mkfifo /data/run.fifo
		truncate -s 16M /data/sparse.img
		printf head | dd of=/data/sparse.img conv=notrunc 2>/dev/null
		touch -d "2021-06-01 12:30:00" /data/config/db.ini /data/config
	'
	docker run --rm --user 0 -v "$volume:/data" alpine:3.20 sh -ec '
		apk add --no-cache attr >/dev/null 2>&1 || exit 0
		setfattr -n user.dvol_test -v acceptance /data/index.html
	' || true

	before="$(tree_manifest "$(mountpoint_of "$volume")")"
	archive="$(dvol_cmd --root "$ARCHIVE_ROOT" backup --compression zstd "$volume" | sed -n 's/^backed up .* -> //p')"
	after="$(tree_manifest "$(mountpoint_of "$volume")")"
	[[ "$before" == "$after" ]] || {
		diff <(printf '%s\n' "$before") <(printf '%s\n' "$after") >&2 || true
		fail "the backup modified the source volume"
	}
	pass "source volume unchanged by the backup"

	restored="$(new_volume fidrestored)"
	dvol_cmd --root "$ARCHIVE_ROOT" restore "$restored" "$archive" >/dev/null
	after="$(tree_manifest "$(mountpoint_of "$restored")")"
	[[ "$before" == "$after" ]] || {
		diff <(printf '%s\n' "$before") <(printf '%s\n' "$after") >&2 || true
		fail "the restored volume differs from the source"
	}
	pass "restored volume identical to the source (mode, owner, ns mtime, xattrs, links, FIFO)"

	# the hole must survive the round trip
	local original_blocks restored_blocks
	original_blocks="$(stat -c %b "$(mountpoint_of "$volume")/sparse.img")"
	restored_blocks="$(stat -c %b "$(mountpoint_of "$restored")/sparse.img")"
	(( restored_blocks <= original_blocks + 64 )) \
		|| fail "sparse file was materialised on restore ($original_blocks -> $restored_blocks blocks)"
	pass "sparse file stayed sparse ($original_blocks -> $restored_blocks blocks)"
}

# ---------------------------------------------------------------------------
# verify
# ---------------------------------------------------------------------------
scenario_verify() {
	echo "verify: healthy and corrupted archives"
	local volume archive
	volume="$(new_volume ver)"
	in_volume "$volume" "printf payload > /data/data.txt; dd if=/dev/urandom of=/data/blob.bin bs=1M count=4 2>/dev/null"
	archive="$(dvol_cmd --root "$ARCHIVE_ROOT" backup "$volume" | sed -n 's/^backed up .* -> //p')"
	dvol_cmd --root "$ARCHIVE_ROOT" verify "$volume" 2>"$WORK_DIR/out.log" >"$WORK_DIR/verify.log" \
		|| fail "verify failed on a healthy archive"
	grep -Fq "OK " "$WORK_DIR/verify.log" || fail "verify did not report success"
	pass "healthy archive verified"

	printf 'corruption' | dd of="$archive" bs=1 seek=512 conv=notrunc 2>/dev/null
	expect_failure "verify of a corrupted archive" \
		dvol_cmd --root "$ARCHIVE_ROOT" verify --archive "$archive"
	expect_output "FAIL"
}

main() {
	command -v docker >/dev/null 2>&1 || fail "Docker is required"
	command -v getfattr >/dev/null 2>&1 || echo "note: getfattr missing, xattr columns will be empty" >&2
	case "$DVOL_MODE" in
		direct) [[ -x "$DVOL_BIN" ]] || fail "build the CLI plugin first: $DVOL_BIN" ;;
		docker) docker dvol --help >/dev/null 2>&1 || fail "docker dvol plugin unavailable" ;;
		*) fail "unsupported DVOL_MODE: $DVOL_MODE" ;;
	esac
	[[ "$(id -u)" == 0 ]] || fail "run this suite as root so Docker mountpoints are readable"
	docker info >/dev/null 2>&1 || fail "Docker Engine is unavailable"
	echo "Defect acceptance dvol mode: $DVOL_MODE"

	for image in alpine:3.20 python:3.12-alpine; do
		docker image inspect "$image" >/dev/null 2>&1 || {
			echo "Pulling test image: $image"
			docker pull "$image" >/dev/null
		}
	done

	mkdir -p -- "$ARCHIVE_ROOT"
	scenario_bundle_into_single_volume
	scenario_hot_backup
	scenario_special_files
	scenario_restore_missing_volume
	scenario_compose_timestamp_lookup
	scenario_compose_volume_set
	scenario_volume_in_use
	scenario_interrupted_restore
	scenario_fidelity
	scenario_verify
	echo "All defect acceptance scenarios passed"
}

main "$@"
