#!/usr/bin/env bash
set -Eeuo pipefail

# Full acceptance suite for the native Docker-volume workflow.
#
# The suite intentionally runs as a real host-side client: docker dvol reads
# the mountpoint returned by Docker, creates missing local volumes, and writes
# archives into a temporary repository. It therefore requires permission to
# read/write Docker volume mountpoints (normally root on Linux).

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/docker-volume-acceptance.XXXXXX")"
readonly SCRIPT_DIR PROJECT_ROOT WORK_DIR
readonly ARCHIVE_ROOT="$WORK_DIR/archives"
readonly ODOO_COMPOSE="$SCRIPT_DIR/acceptance/odoo-compose.yaml"
readonly DVOL_BIN="${DVOL_BIN:-$PROJECT_ROOT/bin/dvol-cli-plugin}"

declare -a DOWNLOADED_IMAGES=()
declare -a CREATED_CONTAINERS=()
declare -a CREATED_VOLUMES=()
declare -a COMPOSE_PROJECTS=()

cleanup() {
	local project image container volume
	set +e

	for project in "${COMPOSE_PROJECTS[@]}"; do
		docker compose -p "$project" -f "$ODOO_COMPOSE" down -v --remove-orphans >/dev/null 2>&1
	done

	for container in "${CREATED_CONTAINERS[@]}"; do
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
	echo "ACCEPTANCE FAILURE: $*" >&2
	return 1
}

require_command() {
	command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

require_image() {
	local image="$1"
	if docker image inspect "$image" >/dev/null 2>&1; then
		return
	fi
	echo "Pulling test image: $image"
	DOWNLOADED_IMAGES+=("$image")
	docker pull "$image" >/dev/null
}

assert_file() {
	[[ -f "$1" ]] || fail "expected file does not exist: $1"
}

assert_equal() {
	local expected="$1" actual="$2" message="$3"
	[[ "$expected" == "$actual" ]] || fail "$message (expected=$expected actual=$actual)"
}

assert_contains() {
	local needle="$1" file="$2" message="$3"
	grep -Fq -- "$needle" "$file" || fail "$message (missing: $needle)"
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

free_port() {
	python3 - <<'PY'
import socket

with socket.socket() as sock:
    sock.bind(("127.0.0.1", 0))
    print(sock.getsockname()[1])
PY
}

volume_mountpoint() {
	docker volume inspect --format '{{.Mountpoint}}' "$1"
}

volume_manifest() {
	local volume="$1" mountpoint
	mountpoint="$(volume_mountpoint "$volume")"
	[[ -d "$mountpoint" ]] || fail "volume mountpoint is not accessible: $volume -> $mountpoint"
	find "$mountpoint" -xdev -mindepth 1 -printf '%P\0' | sort -z |
	while IFS= read -r -d '' relative; do
		local path="$mountpoint/$relative" metadata digest
		metadata="$(stat -c '%F|%a|%u|%g|%s' -- "$path")"
		if [[ -L "$path" ]]; then
			digest="link:$(readlink -- "$path")"
		elif [[ -f "$path" ]]; then
			digest="file:$(sha256sum -- "$path" | awk '{print $1}')"
		else
			digest="special"
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
		*) fail "unsupported archive extension: $archive" ;;
	esac
}

assert_archive_contains() {
	local archive="$1" entry="$2"
	if ! archive_listing "$archive" | sed 's#^\./##' | grep -Fx -- "$entry" >/dev/null; then
		echo "Archive listing for diagnostic ($archive):" >&2
		archive_listing "$archive" | sed -n '1,40p' >&2
		fail "archive $archive does not contain $entry"
	fi
}

backup_single_volume() {
	local volume="$1" compression="$2" archive
	archive="$("$DVOL_BIN" --root "$ARCHIVE_ROOT" backup --compression "$compression" "$volume" \
		| sed -n 's/^backed up .* -> //p')"
	[[ -n "$archive" ]] || fail "backup did not report an archive for $volume"
	assert_file "$archive"
	[[ -s "$archive" ]] || fail "backup archive is empty: $archive"
	echo "$archive"
}

assert_pg_data() {
	local container="$1" expected="$2" actual
	actual="$(docker exec "$container" psql -U postgres -d acceptance -Atqc \
		"SELECT count(*) || ':' || string_agg(id || '=' || payload, ',' ORDER BY id) FROM acceptance_records;")"
	assert_equal "$expected" "$actual" "PostgreSQL data mismatch"
}

run_postgres_scenario() {
	local compression="$1"
	local volume="dvol_accept_pg_${BASHPID}_${compression}"
	local container="dvol-accept-pg-${BASHPID}-${compression}"
	local before after archive expected

	echo "Scenario 1 / compression=$compression"
	docker volume create "$volume" >/dev/null
	CREATED_VOLUMES+=("$volume")
	docker run -d --name "$container" \
		-e POSTGRES_PASSWORD=acceptance \
		-e POSTGRES_DB=acceptance \
		-v "$volume:/var/lib/postgresql/data" \
		postgres:16 >/dev/null
	CREATED_CONTAINERS+=("$container")
	wait_for "PostgreSQL readiness" docker exec "$container" pg_isready -U postgres -d acceptance

	docker exec "$container" psql -U postgres -d acceptance -v ON_ERROR_STOP=1 -c \
		"CREATE TABLE acceptance_records (id integer PRIMARY KEY, payload text NOT NULL); INSERT INTO acceptance_records VALUES (1, 'alpha'), (2, 'beta'), (3, 'gamma');" >/dev/null
	expected="3:1=alpha,2=beta,3=gamma"
	assert_pg_data "$container" "$expected"

	# Quiesce the database before hashing and reading the source volume.
	docker stop "$container" >/dev/null
	before="$(volume_manifest "$volume")"
	archive="$(backup_single_volume "$volume" "$compression")"
	after="$(volume_manifest "$volume")"
	assert_equal "$before" "$after" "source volume changed during backup"

	assert_archive_contains "$archive" "PG_VERSION"
	assert_archive_contains "$archive" "base/"
	"$DVOL_BIN" --root "$ARCHIVE_ROOT" backups "$volume" | grep -Fq -- "$(basename "$archive")" \
		|| fail "backup is not listed by docker dvol backups"

	docker rm "$container" >/dev/null
	docker volume rm "$volume" >/dev/null

	"$DVOL_BIN" --root "$ARCHIVE_ROOT" restore "$volume" "$archive" >/dev/null
	docker run -d --name "$container" \
		-e POSTGRES_PASSWORD=acceptance \
		-e POSTGRES_DB=acceptance \
		-v "$volume:/var/lib/postgresql/data" \
		postgres:16 >/dev/null
	wait_for "restored PostgreSQL readiness" docker exec "$container" pg_isready -U postgres -d acceptance
	assert_pg_data "$container" "$expected"
}

wait_for_odoo() {
	local port="$1"
	wait_for "Odoo HTTP endpoint" curl -fsSL "http://127.0.0.1:${port}/web/database/selector"
	curl -fsSL "http://127.0.0.1:${port}/web/database/selector" | grep -Fq acceptance \
		|| fail "Odoo database selector does not contain the acceptance database"
	curl -fsSL "http://127.0.0.1:${port}/web/login" | grep -Eiq 'log[[:space:]]*in|login' \
		|| fail "Odoo login endpoint is not usable"
}

assert_odoo_data() {
	local project="$1" expected="$2" actual partners
	actual="$(docker compose -p "$project" -f "$ODOO_COMPOSE" exec -T db \
		psql -U odoo -d acceptance -Atqc \
		"SELECT count(*) || ':' || string_agg(payload, ',' ORDER BY id) FROM acceptance_target;")"
	assert_equal "$expected" "$actual" "Odoo database data mismatch"
	partners="$(docker compose -p "$project" -f "$ODOO_COMPOSE" exec -T db \
		psql -U odoo -d acceptance -Atqc "SELECT count(*) FROM res_partner;")"
	[[ "$partners" =~ ^[1-9][0-9]*$ ]] || fail "Odoo base/demo data is missing from res_partner"
	docker compose -p "$project" -f "$ODOO_COMPOSE" exec -T odoo \
		test "$(printf '/var/lib/odoo/filestore/acceptance/acceptance-target.txt')"
}

run_odoo_scenario() {
	local compression="$1"
	local project="dvolaccept${BASHPID}${compression}"
	local port
	port="$(free_port)"
	local archive before_db after_db before_odoo after_odoo
	local db_volume="${project}_db-data" odoo_volume="${project}_odoo-data"
	local expected="2:odoo-alpha,odoo-beta"

	echo "Scenario 2 / compression=$compression"
	export COMPOSE_PROJECT_NAME="$project" ODOO_HTTP_PORT="$port"
	COMPOSE_PROJECTS+=("$project")
	docker compose -p "$project" -f "$ODOO_COMPOSE" up -d
	wait_for "Odoo PostgreSQL readiness" docker compose -p "$project" -f "$ODOO_COMPOSE" exec -T db pg_isready -U odoo -d postgres

	# Create a real Odoo database with the base module and demo records in it.
	docker compose -p "$project" -f "$ODOO_COMPOSE" exec -T db createdb -U odoo acceptance 2>/dev/null || true
	docker compose -p "$project" -f "$ODOO_COMPOSE" exec -T odoo \
		odoo -d acceptance -i base --stop-after-init \
		--db_host=db --db_port=5432 --db_user=odoo --db_password=odoo >/dev/null
	docker compose -p "$project" -f "$ODOO_COMPOSE" exec -T db psql -U odoo -d acceptance -v ON_ERROR_STOP=1 -c \
		"CREATE TABLE acceptance_target (id integer PRIMARY KEY, payload text NOT NULL); INSERT INTO acceptance_target VALUES (1, 'odoo-alpha'), (2, 'odoo-beta');" >/dev/null
	docker compose -p "$project" -f "$ODOO_COMPOSE" exec -T odoo sh -eu -c \
		"mkdir -p /var/lib/odoo/filestore/acceptance; printf 'odoo-filestore-target\\n' > /var/lib/odoo/filestore/acceptance/acceptance-target.txt"
	wait_for_odoo "$port"
	assert_odoo_data "$project" "$expected"

	# Quiesce both services before hashing and backing up their volumes.
	docker compose -p "$project" -f "$ODOO_COMPOSE" stop >/dev/null
	before_db="$(volume_manifest "$db_volume")"
	before_odoo="$(volume_manifest "$odoo_volume")"
	archive="$("$DVOL_BIN" --root "$ARCHIVE_ROOT" compose-backup -f "$ODOO_COMPOSE" -p "$project" \
			--compression "$compression" | sed -n 's/^backed up .* -> //p')"
	[[ -n "$archive" ]] || fail "Compose backup did not report an archive"
	assert_file "$archive"
	[[ -s "$archive" ]] || fail "Compose archive is empty: $archive"
	after_db="$(volume_manifest "$db_volume")"
	after_odoo="$(volume_manifest "$odoo_volume")"
	assert_equal "$before_db" "$after_db" "Odoo database volume changed during backup"
	assert_equal "$before_odoo" "$after_odoo" "Odoo filestore volume changed during backup"
	assert_archive_contains "$archive" "manifest.json"
	assert_archive_contains "$archive" "${db_volume}/"
	assert_archive_contains "$archive" "${odoo_volume}/"

	docker compose -p "$project" -f "$ODOO_COMPOSE" down -v --remove-orphans >/dev/null
	! docker volume inspect "$db_volume" >/dev/null 2>&1 || fail "database volume survived compose down -v"
	! docker volume inspect "$odoo_volume" >/dev/null 2>&1 || fail "filestore volume survived compose down -v"

	"$DVOL_BIN" --root "$ARCHIVE_ROOT" compose-restore -f "$ODOO_COMPOSE" -p "$project" "$archive" >/dev/null
	docker volume inspect "$db_volume" >/dev/null
	docker volume inspect "$odoo_volume" >/dev/null
	after_db="$(volume_manifest "$db_volume")"
	after_odoo="$(volume_manifest "$odoo_volume")"
	assert_equal "$before_db" "$after_db" "restored database volume differs from backup source"
	assert_equal "$before_odoo" "$after_odoo" "restored filestore volume differs from backup source"

	docker compose -p "$project" -f "$ODOO_COMPOSE" up -d
	wait_for "restored Odoo PostgreSQL readiness" docker compose -p "$project" -f "$ODOO_COMPOSE" exec -T db pg_isready -U odoo -d postgres
	wait_for_odoo "$port"
	assert_odoo_data "$project" "$expected"
}

main() {
	local docker_root
	require_command docker
	require_command curl
	require_command python3
	require_command tar
	[[ -x "$DVOL_BIN" ]] || fail "build the CLI plugin first: $DVOL_BIN"
	[[ "$(id -u)" == 0 ]] || fail "run make acceptance as root so Docker volume mountpoints can be verified"
	docker compose version >/dev/null 2>&1 || fail "Docker Compose plugin is required"
	docker_root="$(docker info --format '{{.DockerRootDir}}')"
	[[ -n "$(docker info --format '{{.ServerVersion}}')" ]] || fail "Docker Engine is unavailable"
	if [[ -d "$docker_root/volumes" && ! -r "$docker_root/volumes" ]]; then
		fail "Docker volume mountpoints are not readable; run make acceptance with suitable privileges"
	fi

	mkdir -p -- "$ARCHIVE_ROOT"
	require_image postgres:16
	require_image odoo:17.0

	for compression in gzip xz zstd; do
		run_postgres_scenario "$compression"
		run_odoo_scenario "$compression"
	done
	echo "All acceptance scenarios passed"
}

main "$@"
