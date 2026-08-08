#!/usr/bin/env bash
set -Eeuo pipefail

# Acceptance matrix for the MongoDB Compose and startup scripts supplied in
# test/acceptance. Run as root: docker dvol accesses the Docker mountpoints
# directly so the checks can compare source and restored volume contents.

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/docker-volume-mongo-acceptance.XXXXXX")"
ARCHIVE_ROOT="$WORK_DIR/archives"
MONGO_COMPOSE="$SCRIPT_DIR/acceptance/mongo-compose.yaml"
MONGO_START="$SCRIPT_DIR/acceptance/script-start.mongo.sh"
DVOL_BIN="${DVOL_BIN:-$PROJECT_ROOT/bin/dvol-cli-plugin}"
DVOL_MODE="${DVOL_MODE:-direct}"
readonly SCRIPT_DIR PROJECT_ROOT WORK_DIR ARCHIVE_ROOT MONGO_COMPOSE MONGO_START DVOL_BIN DVOL_MODE

dvol_cmd() {
	case "$DVOL_MODE" in
		direct)
			"$DVOL_BIN" "$@"
			;;
		docker)
			docker dvol "$@"
			;;
		*)
			fail "unsupported DVOL_MODE: $DVOL_MODE (expected direct or docker)"
			;;
	esac
}

declare -a DOWNLOADED_IMAGES=()
declare -a CREATED_VOLUMES=()
declare -a PROBE_CONTAINERS=()
MONGO_CONTAINER_OWNED=0

cleanup() {
	local container image volume
	set +e
	if (( MONGO_CONTAINER_OWNED )); then
		docker rm -f mongodb >/dev/null 2>&1
	fi
	for container in "${PROBE_CONTAINERS[@]}"; do
		docker rm -f "$container" >/dev/null 2>&1
	done
	for volume in "${CREATED_VOLUMES[@]}"; do
		docker volume rm -f "$volume" >/dev/null 2>&1
	done
	for image in "${DOWNLOADED_IMAGES[@]}"; do
		docker image rm "$image" >/dev/null 2>&1
	done
	rm -rf -- "$WORK_DIR"
}
trap cleanup EXIT

fail() {
	echo "MONGO ACCEPTANCE FAILURE: $*" >&2
	return 1
}

assert_equal() {
	local expected="$1" actual="$2" message="$3"
	[[ "$expected" == "$actual" ]] || fail "$message (expected=$expected actual=$actual)"
}

assert_file() {
	[[ -f "$1" ]] || fail "expected file does not exist: $1"
}

wait_for() {
	local description="$1"; shift
	local attempt=0
	while (( attempt < 60 )); do
		((++attempt))
		if "$@" >/dev/null 2>&1; then
			return
		fi
		sleep 2
	done
	fail "timeout waiting for $description"
}

require_image() {
	local image="$1"
	if docker image inspect "$image" >/dev/null 2>&1; then
		return
	fi
	DOWNLOADED_IMAGES+=("$image")
	echo "Pulling test image: $image"
	docker pull "$image" >/dev/null
}

volume_mountpoint() {
	docker volume inspect --format '{{.Mountpoint}}' "$1"
}

# Content manifest: paths, type, mode, owner, size and file/symlink content.
# Timestamps are intentionally excluded because tar extraction can normalize
# them without changing the data being backed up.
volume_manifest() {
	local volume="$1" mountpoint
	mountpoint="$(volume_mountpoint "$volume")"
	[[ -d "$mountpoint" ]] || fail "inaccessible mountpoint: $volume -> $mountpoint"
	find "$mountpoint" -xdev -mindepth 1 -printf '%P\0' | sort -z |
	while IFS= read -r -d '' relative; do
		local path="$mountpoint/$relative" metadata digest
		metadata="$(stat -c '%F|%a|%u|%g|%s' -- "$path")"
		if [[ -L "$path" ]]; then
			digest="link:$(readlink -- "$path")"
		elif [[ -f "$path" ]]; then
			digest="file:$(sha256sum -- "$path" | awk '{print $1}')"
		else
			digest=special
		fi
		printf '%s\t%s\t%s\n' "$relative" "$metadata" "$digest"
	done | sha256sum | awk '{print $1}'
}

archive_listing() {
	local archive="$1"
	case "$archive" in
		*.tar.gz) tar -tzf "$archive" ;;
		*.tar.xz) tar -tJf "$archive" ;;
		*.tar.zst) tar --zstd -tf "$archive" ;;
		*.tar.lz4) return 2 ;; # restore below validates lz4 when host tar lacks lz4 support
		*) fail "unsupported archive: $archive" ;;
	esac
}

assert_archive_contains() {
	local archive="$1" entry="$2"
	if [[ "$archive" == *.tar.lz4 ]]; then
		echo "Skipping lz4 listing check; restore round-trip will validate the archive" >&2
		return
	fi
	archive_listing "$archive" | sed 's#^\./##' | grep -Fx -- "$entry" >/dev/null \
		|| fail "archive $archive does not contain $entry"
}

assert_archive_matches() {
	local archive="$1" pattern="$2"
	if [[ "$archive" == *.tar.lz4 ]]; then
		echo "Skipping lz4 listing check; restore round-trip will validate the archive" >&2
		return
	fi
	if ! archive_listing "$archive" | sed 's#^\./##' | grep -E -- "$pattern" >/dev/null; then
		echo "Archive listing for diagnostic ($archive):" >&2
		archive_listing "$archive" | sed -n '1,80p' >&2
		fail "archive $archive does not contain an entry matching $pattern"
	fi
}

balanced_level() {
	printf '6\n'
}

default_compression_threads() {
	local cpus
	cpus="$(getconf _NPROCESSORS_ONLN 2>/dev/null || printf '1')"
	cpus=$((cpus / 2))
	(( cpus > 0 )) || cpus=1
	printf '%s\n' "$cpus"
}

compression_threads() {
	if [[ "$1" == xz ]]; then
		printf '1\n'
	else
		default_compression_threads
	fi
}

compose() {
	local directory="$1"; shift
	(cd "$directory" && docker compose -p mongoaccept -f "$MONGO_COMPOSE" "$@")
}

start_mongo() {
	local mode="$1" directory="$2"
	if [[ "$mode" == compose ]]; then
		compose "$directory" up -d
	else
		(cd "$directory" && "$MONGO_START")
	fi
	MONGO_CONTAINER_OWNED=1
	wait_for "MongoDB readiness" docker exec mongodb mongosh --quiet \
		--username admin --password changeme --authenticationDatabase admin \
		--eval 'db.adminCommand({ping: 1})'
}

stop_mongo() {
	local mode="$1" directory="$2"
	if [[ "$mode" == compose ]]; then
		compose "$directory" stop >/dev/null
	else
		docker stop mongodb >/dev/null
	fi
}

destroy_mongo() {
	local mode="$1" directory="$2"
	if [[ "$mode" == compose ]]; then
		compose "$directory" down -v --remove-orphans >/dev/null
	else
		docker rm -f mongodb >/dev/null 2>&1 || true
	fi
	docker volume rm -f mongo_data mongo_logs >/dev/null 2>&1 || true
	MONGO_CONTAINER_OWNED=0
}

mongo_eval() {
	docker exec mongodb mongosh --quiet \
		--username admin --password changeme --authenticationDatabase admin \
		--eval "$1"
}

seed_mongo() {
	mongo_eval 'const dbx=db.getSiblingDB("acceptance"); dbx.records.drop(); dbx.records.insertMany([{id:1,payload:"mongo-alpha"},{id:2,payload:"mongo-beta"},{id:3,payload:"mongo-gamma"}]);' >/dev/null
	docker exec mongodb sh -eu -c \
		'printf "mongo-acceptance-log-marker\\n" >> /var/log/mongodb/acceptance-marker.log'
}

assert_mongo_data() {
	local expected actual
	expected='1:mongo-alpha|2:mongo-beta|3:mongo-gamma'
	actual="$(mongo_eval 'const a=db.getSiblingDB("acceptance").records.find({}, {_id:0}).sort({id:1}).toArray(); print(a.map(x => x.id + ":" + x.payload).join("|"));')"
	assert_equal "$expected" "$actual" "Mongo records mismatch"
	mongo_eval 'if (db.getSiblingDB("acceptance").records.countDocuments() !== 3) quit(1);' >/dev/null
		docker exec mongodb sh -eu -c \
		'grep -Fq mongo-acceptance-log-marker /var/log/mongodb/acceptance-marker.log'
}

lock_mongo_for_backup() {
	mongo_eval 'db.fsyncLock();' >/dev/null
}

unlock_mongo_after_backup() {
	mongo_eval 'db.fsyncUnlock();' >/dev/null
}

pause_mongo_for_backup() {
	docker pause mongodb >/dev/null
}

unpause_mongo_after_backup() {
	docker unpause mongodb >/dev/null
}

backup_mongo_volumes() {
	local compression="$1" output archive level threads
	level="$(balanced_level)"
	threads="$(compression_threads "$compression")"
	output="$(dvol_cmd --root "$ARCHIVE_ROOT" backup --compression "$compression" --level "$level" --threads "$threads" mongo_data mongo_logs)"
	mapfile -t ARCHIVES < <(printf '%s\n' "$output" | sed -n 's/^backed up .* -> //p')
	[[ ${#ARCHIVES[@]} -eq 2 ]] || fail "expected 2 backup archives, got ${#ARCHIVES[@]}"
	DATA_ARCHIVE=""
	LOGS_ARCHIVE=""
	for archive in "${ARCHIVES[@]}"; do
		assert_file "$archive"
		[[ -s "$archive" ]] || fail "empty archive: $archive"
		case "$archive" in
			*/mongo_data/*) DATA_ARCHIVE="$archive" ;;
			*/mongo_logs/*) LOGS_ARCHIVE="$archive" ;;
			*) fail "archive path is not associated with a Mongo volume: $archive" ;;
		esac
	done
	[[ -n "$DATA_ARCHIVE" && -n "$LOGS_ARCHIVE" ]] || fail "missing data or log archive"
	assert_archive_matches "$DATA_ARCHIVE" '(^|/)WiredTiger(\.|$)'
	assert_archive_contains "$LOGS_ARCHIVE" mongod.log
	assert_archive_contains "$LOGS_ARCHIVE" acceptance-marker.log
	dvol_cmd --root "$ARCHIVE_ROOT" backups mongo_data \
		| grep -F -- "$(basename "$DATA_ARCHIVE")" >/dev/null \
		|| fail "mongo_data archive is not listed"
	dvol_cmd --root "$ARCHIVE_ROOT" backups mongo_logs \
		| grep -F -- "$(basename "$LOGS_ARCHIVE")" >/dev/null \
		|| fail "mongo_logs archive is not listed"
}

backup_flat_mongo_volumes() {
	local compression="$1" destination="$2" output archive stamp level threads
	level="$(balanced_level)"
	threads="$(compression_threads "$compression")"
	mkdir -p "$destination"
	output="$(dvol_cmd --destination "$destination" backup \
		--compression "$compression" --level "$level" --threads "$threads" \
		--archive-name mongo_data::mongo-flat-data \
		--archive-name mongo_logs::mongo-flat-logs \
		--same-destination --timestamp mongo_data mongo_logs)"
	mapfile -t FLAT_ARCHIVES < <(printf '%s\n' "$output" | sed -n 's/^backed up .* -> //p')
	[[ ${#FLAT_ARCHIVES[@]} -eq 2 ]] || fail "expected 2 flat backup archives, got ${#FLAT_ARCHIVES[@]}"
	FLAT_DATA_ARCHIVE=""
	FLAT_LOGS_ARCHIVE=""
	for archive in "${FLAT_ARCHIVES[@]}"; do
		assert_file "$archive"
		[[ "$(dirname -- "$archive")" == "$destination" ]] || fail "archive is not flat: $archive"
		[[ "$(stat -c '%a' -- "$archive")" == 644 ]] || fail "unexpected archive permissions: $archive"
		case "$(basename -- "$archive")" in
			mongo-flat-data_*) FLAT_DATA_ARCHIVE="$archive" ;;
			mongo-flat-logs_*) FLAT_LOGS_ARCHIVE="$archive" ;;
			*) fail "unexpected flat archive name: $archive" ;;
		esac
	done
	[[ -n "$FLAT_DATA_ARCHIVE" && -n "$FLAT_LOGS_ARCHIVE" ]] || fail "missing flat Mongo archives"
	stamp="$(basename -- "$FLAT_DATA_ARCHIVE")"
	stamp="${stamp#mongo-flat-data_}"
	stamp="${stamp%.tar.*}"
	[[ "$(basename -- "$FLAT_LOGS_ARCHIVE")" == "mongo-flat-logs_${stamp}.tar."* ]] \
		|| fail "flat archives do not share one timestamp"
	[[ -f "$destination/.dvol-index.json" ]] || fail "flat backup metadata index is missing"
	[[ "$(stat -c '%a' -- "$destination/.dvol-index.json")" == 644 ]] \
		|| fail "unexpected flat metadata permissions"
	dvol_cmd --destination "$destination" backups mongo_data \
		| grep -F -- "$(basename -- "$FLAT_DATA_ARCHIVE")" >/dev/null \
		|| fail "flat mongo_data archive is not listed"
	dvol_cmd --destination "$destination" backups mongo_logs \
		| grep -F -- "$(basename -- "$FLAT_LOGS_ARCHIVE")" >/dev/null \
		|| fail "flat mongo_logs archive is not listed"
}

write_existing_target_sentinels() {
	docker volume create mongo_data >/dev/null
	docker volume create mongo_logs >/dev/null
	docker run --rm --user 0 --entrypoint sh -v mongo_data:/data busybox:1.36 -eu -c \
		'printf sentinel > /data/acceptance-sentinel'
	docker run --rm --user 0 --entrypoint sh -v mongo_logs:/data busybox:1.36 -eu -c \
		'printf sentinel > /data/acceptance-sentinel'
}

assert_sentinels_removed() {
	docker run --rm --user 0 --entrypoint sh -v mongo_data:/data busybox:1.36 -eu -c \
		'test ! -e /data/acceptance-sentinel'
	docker run --rm --user 0 --entrypoint sh -v mongo_logs:/data busybox:1.36 -eu -c \
		'test ! -e /data/acceptance-sentinel'
}

restore_same_name_and_verify() {
	local mode="$1" directory="$2" data_archive="$3" logs_archive="$4"
	write_existing_target_sentinels
	dvol_cmd --root "$ARCHIVE_ROOT" restore mongo_data "$data_archive" >/dev/null
	dvol_cmd --root "$ARCHIVE_ROOT" restore mongo_logs "$logs_archive" >/dev/null
	assert_sentinels_removed
	start_mongo "$mode" "$directory"
	assert_mongo_data
	stop_mongo "$mode" "$directory"
	destroy_mongo "$mode" "$directory"
}

restore_same_name_from_flat_and_verify() {
	local mode="$1" directory="$2" destination="$3" data_archive="$4" logs_archive="$5"
	write_existing_target_sentinels
	dvol_cmd --destination "$destination" restore mongo_data "$(basename -- "$data_archive")" >/dev/null
	dvol_cmd --destination "$destination" restore mongo_logs "$(basename -- "$logs_archive")" >/dev/null
	assert_sentinels_removed
	start_mongo "$mode" "$directory"
	assert_mongo_data
	stop_mongo "$mode" "$directory"
	destroy_mongo "$mode" "$directory"
}

probe_restored_copy() {
	local data_copy="$1" logs_copy="$2" probe="mongo-accept-probe-${BASHPID}"
	PROBE_CONTAINERS+=("$probe")
	docker run -d --name "$probe" \
		-v "$data_copy:/probe-data" mongo:7 \
		mongod --dbpath /probe-data --bind_ip_all --logpath /dev/null >/dev/null
	wait_for "restored MongoDB copy" docker exec "$probe" mongosh --quiet \
		--eval 'db.getSiblingDB("acceptance").records.countDocuments() === 3 ? 0 : quit(1)'
	docker exec "$probe" mongosh --quiet --eval \
		'const a=db.getSiblingDB("acceptance").records.find({}, {_id:0}).sort({id:1}).toArray(); if (a[0].payload !== "mongo-alpha" || a[2].payload !== "mongo-gamma") quit(1);'
	docker run --rm --user 0 --entrypoint sh -v "$logs_copy:/data:ro" busybox:1.36 -eu -c \
		'grep -Fq mongo-acceptance-log-marker /data/acceptance-marker.log'
	docker rm -f "$probe" >/dev/null
}

restore_renamed_copies_and_verify() {
	local data_archive="$1" logs_archive="$2" suffix="$3"
	local data_copy="mongo_data_copy_${suffix}" logs_copy="mongo_logs_copy_${suffix}"
	docker volume create "$data_copy" >/dev/null
	docker volume create "$logs_copy" >/dev/null
	CREATED_VOLUMES+=("$data_copy" "$logs_copy")
	dvol_cmd --root "$ARCHIVE_ROOT" restore "$data_copy" "$data_archive" >/dev/null
	dvol_cmd --root "$ARCHIVE_ROOT" restore "$logs_copy" "$logs_archive" >/dev/null
	probe_restored_copy "$data_copy" "$logs_copy"
	docker volume rm -f "$data_copy" "$logs_copy" >/dev/null
}

restore_missing_copies_and_verify() {
	local data_archive="$1" logs_archive="$2" suffix="$3"
	local data_copy="mongo_data_missing_${suffix}" logs_copy="mongo_logs_missing_${suffix}"
	! docker volume inspect "$data_copy" >/dev/null 2>&1 || fail "test target already exists: $data_copy"
	! docker volume inspect "$logs_copy" >/dev/null 2>&1 || fail "test target already exists: $logs_copy"
	CREATED_VOLUMES+=("$data_copy" "$logs_copy")
	dvol_cmd --root "$ARCHIVE_ROOT" restore "$data_copy" "$data_archive" >/dev/null
	dvol_cmd --root "$ARCHIVE_ROOT" restore "$logs_copy" "$logs_archive" >/dev/null
	probe_restored_copy "$data_copy" "$logs_copy"
	docker volume rm -f "$data_copy" "$logs_copy" >/dev/null
}

run_case() {
	local mode compression directory
	local before_data before_logs after_data after_logs
	local data_archive logs_archive flat_directory
	mode="$1"
	compression="$2"
	directory="$WORK_DIR/${mode}-${compression}"
	mkdir -p "$directory/volumes/mongo_data" "$directory/volumes/mongo_logs"
	CREATED_VOLUMES+=(mongo_data mongo_logs)

	echo "Mongo matrix: bootstrap=$mode compression=$compression"
	start_mongo "$mode" "$directory"
	seed_mongo
	assert_mongo_data
	# Keep the bind-backed volume mounted while dvol reads its Docker
	# mountpoint. fsyncLock plus pause makes this read-only window deterministic.
	lock_mongo_for_backup
	pause_mongo_for_backup
	before_data="$(volume_manifest mongo_data)"
	before_logs="$(volume_manifest mongo_logs)"
	backup_mongo_volumes "$compression"
	data_archive="$DATA_ARCHIVE"
	logs_archive="$LOGS_ARCHIVE"
	flat_directory="$directory/flat-archives"
	backup_flat_mongo_volumes "$compression" "$flat_directory"
	after_data="$(volume_manifest mongo_data)"
	after_logs="$(volume_manifest mongo_logs)"
	assert_equal "$before_data" "$after_data" "mongo_data changed during backup"
	assert_equal "$before_logs" "$after_logs" "mongo_logs changed during backup"
	unpause_mongo_after_backup
	unlock_mongo_after_backup
	stop_mongo "$mode" "$directory"

	destroy_mongo "$mode" "$directory"
	restore_same_name_from_flat_and_verify "$mode" "$directory" "$flat_directory" "$FLAT_DATA_ARCHIVE" "$FLAT_LOGS_ARCHIVE"
	restore_renamed_copies_and_verify "$data_archive" "$logs_archive" "${mode}_${compression}"
	restore_missing_copies_and_verify "$data_archive" "$logs_archive" "${mode}_${compression}"
}

preflight() {
	command -v docker >/dev/null 2>&1 || fail "Docker is required"
	command -v tar >/dev/null 2>&1 || fail "tar is required"
	command -v python3 >/dev/null 2>&1 || fail "python3 is required"
	case "$DVOL_MODE" in
		direct)
			[[ -x "$DVOL_BIN" ]] || fail "build the CLI plugin first: $DVOL_BIN"
			;;
		docker)
		docker dvol --help >/dev/null 2>&1 \
			|| fail "the installed docker dvol CLI plugin is unavailable"
			;;
		*)
			fail "unsupported DVOL_MODE: $DVOL_MODE (expected direct or docker)"
			;;
	esac
	echo "Mongo acceptance dvol mode: $DVOL_MODE"
	[[ "$(id -u)" == 0 ]] || fail "run this suite as root"
	docker compose version >/dev/null 2>&1 || fail "Docker Compose plugin is required"
	docker info >/dev/null 2>&1 || fail "Docker Engine is unavailable"
	for volume in mongo_data mongo_logs; do
		! docker volume inspect "$volume" >/dev/null 2>&1 \
			|| fail "refusing to touch pre-existing volume: $volume"
	done
	! docker ps -a --format '{{.Names}}' | grep -Fx mongodb >/dev/null \
		|| fail "refusing to touch pre-existing container: mongodb"
	python3 - <<'PY' || fail 'TCP port 27017 is already in use'
import socket
with socket.socket() as sock:
    sock.bind(("127.0.0.1", 27017))
PY
}

main() {
	preflight
	mkdir -p "$ARCHIVE_ROOT"
	require_image mongo:7
	require_image busybox:1.36
	for compression in gzip xz zstd lz4; do
		run_case compose "$compression"
		run_case script "$compression"
	done
	echo "All MongoDB acceptance scenarios passed"
}

main "$@"
