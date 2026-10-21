#!/usr/bin/env bash
# shellcheck disable=SC2016 # single quotes intentionally defer expansion to the container shell
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
	# new_volume runs in a command substitution, so its CREATED_VOLUMES entry
	# never reaches this shell; the per-run name prefix finds those volumes.
	docker volume ls -q | grep "^dvol_${TAG}_" | while IFS= read -r volume; do
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

lock_container_name() {
	local digest
	digest="$(printf '%s' "$1" | sha256sum | awk '{print $1}')"
	printf 'dvol-lock-%s\n' "${digest:0:24}"
}

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

	# A tar path equivalent to manifest.json must not evade classification just
	# because it has more than one leading ./ component.
	local noncanonical="$WORK_DIR/noncanonical-bundle.tar.gz"
	docker run --rm -v "$WORK_DIR:/work" python:3.12-alpine python -c '
import io, json, sys, tarfile
payload = json.dumps({"format":"dvol-bundle-v1", "volumes":sys.argv[1:]}).encode()
with tarfile.open("/work/noncanonical-bundle.tar.gz", "w:gz", format=tarfile.PAX_FORMAT) as archive:
    header = tarfile.TarInfo("././manifest.json")
    header.size = len(payload)
    header.mode = 0o644
    archive.addfile(header, io.BytesIO(payload))
' "$a" "$b"
	expect_failure "restoring a non-canonical bundle as a single volume" \
		dvol_cmd --root "$ARCHIVE_ROOT" restore "$victim" "$noncanonical"
	expect_output "multi-volume dvol bundle"
	[[ "$before" == "$(manifest_digest "$(mountpoint_of "$victim")")" ]] \
		|| fail "the non-canonical bundle modified the victim volume"
	pass "non-canonical manifest classified before publication"

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

	local failed="dvol_${TAG}_h1failed" corrupt="$WORK_DIR/corrupt.tar.gz"
	CREATED_VOLUMES+=("$failed")
	printf 'incomplete archive' >"$corrupt"
	expect_failure "failed restore rolls back a newly created Docker volume" \
		dvol_cmd --root "$ARCHIVE_ROOT" restore "$failed" "$corrupt"
	if docker volume inspect "$failed" >/dev/null 2>&1; then
		fail "failed restore left the newly created Docker volume behind"
	fi
	pass "failed restore removed its empty Docker volume"
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
# Volume lock: Docker itself must refuse direct, forced and Compose removal.
# ---------------------------------------------------------------------------
scenario_volume_lock() {
	echo "lock: protected volumes survive Docker removal commands"
	local volume guard archive dir project compose_volume compose_guard compose_container

	volume="$(new_volume locked)"
	in_volume "$volume" "printf original > /data/payload.txt"
	archive="$(dvol_cmd --root "$ARCHIVE_ROOT" backup "$volume" | sed -n 's/^backed up .* -> //p')"
	dvol_cmd lock "$volume" >"$WORK_DIR/lock.log"
	guard="$(lock_container_name "$volume")"
	CREATED_CONTAINERS+=("$guard")
	[[ "$(docker inspect --format '{{.State.Running}}' "$guard")" == true ]] \
		|| fail "volume lock guard is not running"
	dvol_cmd lock "$volume" >"$WORK_DIR/relock.log"
	grep -Fq "already locked" "$WORK_DIR/relock.log" \
		|| fail "second lock did not report its idempotent state"
	in_volume "$volume" "printf changed > /data/payload.txt"
	dvol_cmd --root "$ARCHIVE_ROOT" restore "$volume" "$archive" >/dev/null
	[[ "$(cat "$(mountpoint_of "$volume")/payload.txt")" == original ]] \
		|| fail "dvol lock guard incorrectly blocked or corrupted restore"
	pass "lock guard excluded from application-container restore checks"

	expect_failure "docker volume rm of a locked volume" docker volume rm "$volume"
	expect_output "in use"
	expect_failure "docker volume rm -f of a locked volume" docker volume rm -f "$volume"
	expect_output "in use"
	docker volume inspect "$volume" >/dev/null \
		|| fail "locked volume disappeared after direct removal attempts"
	dvol_cmd unlock "$volume" >"$WORK_DIR/unlock.log"
	dvol_cmd unlock "$volume" >"$WORK_DIR/reunlock.log"
	grep -Fq "already unlocked" "$WORK_DIR/reunlock.log" \
		|| fail "second unlock did not report its idempotent state"
	docker volume rm "$volume" >/dev/null
	pass "direct and forced removal refused until unlock"

	project="${TAG}lock"
	dir="$WORK_DIR/$project"
	compose_volume="${project}_data"
	compose_container="${project}-app-1"
	mkdir -p "$dir"
	cat >"$dir/compose.yaml" <<YAML
name: $project
services:
  app:
    image: busybox:1.36.1
    command: ["tail", "-f", "/dev/null"]
    volumes:
      - data:/data
volumes:
  data:
YAML
	docker compose -f "$dir/compose.yaml" up -d >/dev/null
	CREATED_VOLUMES+=("$compose_volume")
	CREATED_CONTAINERS+=("$compose_container")
	dvol_cmd lock "$compose_volume" >/dev/null
	compose_guard="$(lock_container_name "$compose_volume")"
	CREATED_CONTAINERS+=("$compose_guard")

	# Compose 5.x reports the failed volume removal but still exits zero. The
	# observable contract is that Docker refuses the removal and the volume
	# remains; a CLI plugin cannot override Compose's exit-code policy.
	docker compose -f "$dir/compose.yaml" down -v --remove-orphans \
		>"$WORK_DIR/out.log" 2>&1 || true
	expect_output "Resource is still in use"
	docker volume inspect "$compose_volume" >/dev/null \
		|| fail "compose removed a locked volume"
	dvol_cmd unlock "$compose_volume" >/dev/null
	docker volume rm "$compose_volume" >/dev/null
	docker compose -f "$dir/compose.yaml" down --remove-orphans >/dev/null 2>&1 || true
	pass "compose down -v refused the locked volume until unlock"
}

# ---------------------------------------------------------------------------
# H6: an interrupted restore must leave the volume recoverable.
# ---------------------------------------------------------------------------
scenario_interrupted_restore() {
	echo "H6: restore killed mid-flight is repaired on the next run"
	local volume archive mountpoint status delay
	volume="$(new_volume h6)"
	in_volume "$volume" "printf original > /data/marker.txt; dd if=/dev/urandom of=/data/payload.bin bs=1M count=192 2>/dev/null"
	archive="$(dvol_cmd --root "$ARCHIVE_ROOT" backup --compression lz4 "$volume" | sed -n 's/^backed up .* -> //p')"
	mountpoint="$(mountpoint_of "$volume")"

	# Without --foreground GNU timeout sends the signal to its whole process
	# group: SIGKILL takes timeout down with dvol and the shell resumes while dvol
	# is still exiting. A thread blocked in syncfs keeps the journal record
	# flocked, so the recovery below rightly treats it as in flight and the
	# staging directory survives until a later run. With --foreground timeout
	# kills dvol alone and reaps it, and the lock is gone before recovery starts.
	#
	# A fast host finishes the restore before the deadline, which tests nothing,
	# so the deadline shrinks until a kill actually lands.
	status=0
	for delay in 1 0.5 0.25 0.1 0.05; do
		in_volume "$volume" "printf live > /data/marker.txt"
		set +e
		timeout --foreground -s KILL "$delay" "$DVOL_BIN" --root "$ARCHIVE_ROOT" restore --force "$volume" "$archive" >/dev/null 2>&1
		status=$?
		set -e
		[[ "$status" == 0 ]] || break
	done
	echo "  interrupted restore exited with $status after ${delay}s"
	[[ "$status" == 137 ]] || fail "no restore attempt was interrupted (last exit status $status)"

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
# R5 (2026-09-11): the Docker CLI writes advisory lines to stderr on a healthy
# daemon. dvol merged them into stdout and failed to parse its own output.
# ---------------------------------------------------------------------------
scenario_docker_stderr_warnings() {
	echo "R5: Docker CLI warnings on stderr must not break dvol"
	local volume shim_dir real_docker archive
	volume="$(new_volume r5)"
	in_volume "$volume" "printf warned > /data/marker.txt"

	real_docker="$(command -v docker)"
	shim_dir="$WORK_DIR/docker-shim"
	mkdir -p -- "$shim_dir"
	{
		echo '#!/usr/bin/env bash'
		echo 'echo "WARNING: API version negotiation failed, using default" >&2'
		echo "exec $real_docker \"\$@\""
	} >"$shim_dir/docker"
	chmod +x "$shim_dir/docker"

	( export PATH="$shim_dir:$PATH"
	  dvol_cmd --root "$ARCHIVE_ROOT" list >/dev/null ) \
		|| fail "list failed while the Docker CLI emitted a stderr warning"
	archive="$( ( export PATH="$shim_dir:$PATH"
	              dvol_cmd --root "$ARCHIVE_ROOT" backup "$volume" ) | sed -n 's/^backed up .* -> //p')"
	[[ -n "$archive" ]] || fail "backup failed while the Docker CLI emitted a stderr warning"
	in_volume "$volume" "printf changed > /data/marker.txt"
	( export PATH="$shim_dir:$PATH"
	  dvol_cmd --root "$ARCHIVE_ROOT" restore --force "$volume" "$archive" >/dev/null ) \
		|| fail "restore failed while the Docker CLI emitted a stderr warning"
	in_volume "$volume" 'test "$(cat /data/marker.txt)" = warned' \
		|| fail "restore did not reproduce the archived content"
	pass "backup, list and restore unaffected by Docker stderr warnings"
}

# ---------------------------------------------------------------------------
# R1 (2026-09-11): the temporary-archive coordination lock was written to the
# parent of the destination, outside the directory dvol was pointed at.
# ---------------------------------------------------------------------------
scenario_destination_control_files() {
	echo "R1: dvol control files stay inside the destination"
	local volume destination parent
	volume="$(new_volume r1)"
	in_volume "$volume" "printf payload > /data/marker.txt"
	parent="$WORK_DIR/share"
	destination="$parent/backups"
	mkdir -p -- "$destination"

	dvol_cmd --root "$ARCHIVE_ROOT" --destination "$destination" \
		backup --same-destination "$volume" >/dev/null
	dvol_cmd --root "$ARCHIVE_ROOT" --destination "$destination" backup "$volume" >/dev/null

	[[ -f "$destination/.dvol-temp.lock" ]] \
		|| fail "the temporary-archive lock is not inside the destination"
	[[ ! -e "$parent/.dvol-temp.lock" ]] \
		|| fail "dvol wrote its temporary-archive lock outside the destination"
	[[ -z "$(find "$destination" -maxdepth 1 -name '.dvol-backup-*' -print -quit)" ]] \
		|| fail "a temporary archive was left behind"
	pass "control files confined to the destination directory"

	# The lock must never be mistaken for a backup.
	dvol_cmd --root "$ARCHIVE_ROOT" --destination "$destination" backups "$volume" \
		| grep -q '\.dvol-temp\.lock' && fail "the coordination lock is listed as a backup"
	pass "the coordination lock is not listed as a backup"
}

# ---------------------------------------------------------------------------
# R3/R4 (2026-09-11): another dvol process merely starting up opened the
# replacement journal of an in-flight restore, and either stole its record or
# aborted with "recover interrupted volume replacement: no such file or
# directory".
# ---------------------------------------------------------------------------
scenario_concurrent_store_opens() {
	echo "R3/R4: concurrent dvol invocations must not disturb a running restore"
	local volume archive log pid status
	volume="$(new_volume r3)"
	in_volume "$volume" "printf original > /data/marker.txt; dd if=/dev/urandom of=/data/payload.bin bs=1M count=96 2>/dev/null"
	archive="$(dvol_cmd --root "$ARCHIVE_ROOT" backup --compression lz4 "$volume" | sed -n 's/^backed up .* -> //p')"
	in_volume "$volume" "printf live > /data/marker.txt"
	log="$WORK_DIR/concurrent-opens.log"

	(
		for _ in $(seq 1 40); do
			dvol_cmd --root "$ARCHIVE_ROOT" backups "$volume" >/dev/null 2>>"$log" || echo "backups failed" >>"$log"
		done
	) &
	pid=$!
	set +e
	dvol_cmd --root "$ARCHIVE_ROOT" restore --force "$volume" "$archive" >/dev/null 2>>"$log"
	status=$?
	set -e
	wait "$pid"
	[[ $status -eq 0 ]] || fail "the restore failed while other dvol processes were opening the store"
	if [[ -s "$log" ]] && grep -qE 'replacement journal|interrupted volume replacement|backups failed' "$log"; then
		cat "$log" >&2
		fail "a concurrent store open disturbed the in-flight restore"
	fi
	in_volume "$volume" 'test "$(cat /data/marker.txt)" = original' \
		|| fail "the restore did not publish the archived content"
	pass "restore unaffected by 40 concurrent store opens"
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
		apk add --no-cache attr >/dev/null
		setfattr -n user.dvol_test -v acceptance /data/index.html
	'
	[[ "$(getfattr -n user.dvol_test --only-values -- "$(mountpoint_of "$volume")/index.html")" == "acceptance" ]] \
		|| fail "the fidelity fixture could not create its required xattr"

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
	command -v getfattr >/dev/null 2>&1 || fail "required command not found: getfattr (install the attr package)"
	case "$DVOL_MODE" in
		direct) [[ -x "$DVOL_BIN" ]] || fail "build the CLI plugin first: $DVOL_BIN" ;;
		docker) docker dvol --help >/dev/null 2>&1 || fail "docker dvol plugin unavailable" ;;
		*) fail "unsupported DVOL_MODE: $DVOL_MODE" ;;
	esac
	[[ "$(id -u)" == 0 ]] || fail "run this suite as root so Docker mountpoints are readable"
	docker info >/dev/null 2>&1 || fail "Docker Engine is unavailable"
	echo "Defect acceptance dvol mode: $DVOL_MODE"

	for image in alpine:3.20 python:3.12-alpine busybox:1.36.1; do
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
	scenario_volume_lock
	scenario_volume_in_use
	scenario_interrupted_restore
	scenario_docker_stderr_warnings
	scenario_destination_control_files
	scenario_concurrent_store_opens
	scenario_fidelity
	scenario_verify
	echo "All defect acceptance scenarios passed"
}

main "$@"
