#!/usr/bin/env bash
#
# Smoke-test the production container image.
#
# It proves the properties the image is supposed to guarantee rather than just
# that it starts: non-root execution, no build toolchain, working liveness and
# readiness endpoints, a working OCI health check, fail-fast configuration
# validation, an owner-only database created on a mounted data volume, and a
# clean exit on SIGTERM.
#
# Usage:
#   scripts/smoke-test.sh              # build the image, then test it
#   IMAGE=ghcr.io/... BUILD=0 scripts/smoke-test.sh   # test an existing image

set -euo pipefail

IMAGE="${IMAGE:-pacapulse:smoke}"
BUILD="${BUILD:-1}"
# Bounds the wait for the container to answer probes, and the termination grace
# period given to `docker stop`.
STARTUP_TIMEOUT="${STARTUP_TIMEOUT:-30}"
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-60}"
STOP_TIMEOUT="${STOP_TIMEOUT:-30}"

CONTAINER="pacapulse-smoke-$$"
EXPORT_CONTAINER="${CONTAINER}-fs"
WORKDIR="$(mktemp -d)"

pass() { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1" >&2; exit 1; }
step() { printf '\n==> %s\n' "$1"; }

cleanup() {
  docker rm --force "$CONTAINER" >/dev/null 2>&1 || true
  docker rm --force "$EXPORT_CONTAINER" >/dev/null 2>&1 || true
  # The service creates its data directory owner-only and as the container's
  # user, not this one, so this user cannot descend into it to delete it —
  # which is the property the test just proved. Remove everything else and
  # leave that directory to the temporary filesystem it lives in, rather than
  # failing a passing run on its teardown.
  rm -rf "$WORKDIR" 2>/dev/null || true
}
trap cleanup EXIT

for tool in docker curl tar; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required but not installed"
done

if [ "$BUILD" = "1" ]; then
  step "Building $IMAGE"
  docker build \
    --build-arg "VERSION=$(git rev-parse HEAD 2>/dev/null || echo dev)" \
    --tag "$IMAGE" \
    .
fi

step "Inspecting the runtime image"

user="$(docker image inspect --format '{{.Config.User}}' "$IMAGE")"
case "$user" in
  ""|root|0|0:0|root:root) fail "image runs as root (User=${user:-unset})" ;;
  *) pass "image runs as non-root user '$user'" ;;
esac

entrypoint="$(docker image inspect --format '{{json .Config.Entrypoint}}' "$IMAGE")"
case "$entrypoint" in
  *pacapulse*) pass "entrypoint is the service binary: $entrypoint" ;;
  *) fail "unexpected entrypoint: $entrypoint" ;;
esac

# Listing the filesystem is stronger than probing for a `go` binary, because the
# runtime image has no shell to probe with.
docker create --name "$EXPORT_CONTAINER" "$IMAGE" >/dev/null
docker export "$EXPORT_CONTAINER" | tar -t > "$WORKDIR/layout.txt"
docker rm "$EXPORT_CONTAINER" >/dev/null

if ! grep -qx 'usr/local/bin/pacapulse' "$WORKDIR/layout.txt"; then
  fail "the service binary is missing from the image"
fi
pass "the service binary is present at /usr/local/bin/pacapulse"

if grep -qE '^usr/local/go/|/go$|/gofmt$|/godoc$' "$WORKDIR/layout.txt"; then
  fail "the runtime image contains Go toolchain files"
fi
pass "the runtime image contains no Go toolchain"

step "Checking version reporting"
version="$(docker run --rm "$IMAGE" -version)"
[ -n "$version" ] || fail "-version printed nothing"
pass "image reports version '$version'"

step "Rejecting invalid configuration"
set +e
invalid_output="$(docker run --rm "$IMAGE" 2>&1)"
invalid_code=$?
set -e
[ "$invalid_code" -eq 2 ] || fail "expected exit code 2 without required configuration, got $invalid_code"
case "$invalid_output" in
  *PACAPULSE_BASE_URL*) pass "startup failed with exit code 2 naming PACAPULSE_BASE_URL" ;;
  *) fail "configuration error did not name the missing variable: $invalid_output" ;;
esac

set +e
secret_output="$(docker run --rm \
  -e PACAPULSE_BASE_URL=https://pulse.example.com \
  -e PACAPULSE_LOG_LEVEL=not-a-level-and-a-secret \
  "$IMAGE" 2>&1)"
secret_code=$?
set -e
[ "$secret_code" -eq 2 ] || fail "expected exit code 2 for an invalid log level, got $secret_code"
case "$secret_output" in
  *not-a-level-and-a-secret*) fail "configuration error echoed the configured value: $secret_output" ;;
  *) pass "configuration error names the variable without echoing its value" ;;
esac

step "Starting the container"
# The database belongs on a mounted volume, so the smoke test provides one the
# way a PVC would. The mount point is world-writable because the container runs
# as uid 65532; the directory the service creates beneath it is the one whose
# permissions this test then checks.
MOUNT="$WORKDIR/volume"
mkdir -p "$MOUNT"
chmod 0777 "$MOUNT"

docker run --detach \
  --name "$CONTAINER" \
  --env PACAPULSE_BASE_URL=https://pulse.example.com \
  --env PACAPULSE_DATA_DIR=/data/pacapulse \
  --volume "$MOUNT:/data" \
  --publish 127.0.0.1::8080 \
  "$IMAGE" >/dev/null

published="$(docker port "$CONTAINER" 8080/tcp | head -n 1)"
port="${published##*:}"
[ -n "$port" ] || fail "could not determine the published port"
base="http://127.0.0.1:${port}"
pass "container listening on $base"

probe() {
  local path="$1" deadline=$((SECONDS + STARTUP_TIMEOUT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if curl --fail --silent --show-error --max-time 2 "${base}${path}" >"$WORKDIR/body.json" 2>/dev/null; then
      return 0
    fi
    sleep 1
  done
  return 1
}

step "Exercising the health endpoints"
probe /healthz || fail "liveness endpoint did not become available within ${STARTUP_TIMEOUT}s"
grep -q '"status":"alive"' "$WORKDIR/body.json" || fail "unexpected liveness body: $(cat "$WORKDIR/body.json")"
pass "GET /healthz returned 200 and reported the process alive"

probe /readyz || fail "readiness endpoint did not become available within ${STARTUP_TIMEOUT}s"
grep -q '"status":"ready"' "$WORKDIR/body.json" || fail "unexpected readiness body: $(cat "$WORKDIR/body.json")"
pass "GET /readyz returned 200 and reported the instance ready"

step "Waiting for the OCI health check"
deadline=$((SECONDS + HEALTH_TIMEOUT))
health=""
while [ "$SECONDS" -lt "$deadline" ]; do
  health="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$CONTAINER")"
  [ "$health" = "healthy" ] && break
  [ "$health" = "unhealthy" ] && break
  sleep 1
done
[ "$health" = "healthy" ] || fail "container health status is '${health:-none}', want 'healthy'"
pass "the image's HEALTHCHECK reports healthy"

step "Checking the mounted data volume"
[ -d "$MOUNT/pacapulse" ] || fail "the service did not create its data directory on the volume"
pass "the service created its data directory on the mounted volume"

step "Rejecting a second writer"
# ReadWriteOnce is node-scoped and is not a process-level guarantee, so the
# service refuses to start while another process holds the volume's lock.
set +e
second_output="$(docker run --rm \
  --env PACAPULSE_BASE_URL=https://pulse.example.com \
  --env PACAPULSE_DATA_DIR=/data/pacapulse \
  --volume "$MOUNT:/data" \
  "$IMAGE" 2>&1)"
second_code=$?
set -e
[ "$second_code" -eq 1 ] || fail "a second writer exited with code $second_code, want 1: $second_output"
case "$second_output" in
  */data/pacapulse*) fail "the refusal named the data directory: $second_output" ;;
  *) pass "a second container was refused the volume without naming its path" ;;
esac

step "Checking log privacy"
docker logs "$CONTAINER" >"$WORKDIR/logs.txt" 2>&1
for forbidden in 'pulse.example.com' 'PACAPULSE_BASE_URL' '/data/pacapulse' 'User-Agent' 'curl/'; do
  if grep -qF "$forbidden" "$WORKDIR/logs.txt"; then
    fail "container logs contain '$forbidden'"
  fi
done
pass "container logs contain no configuration values or request metadata"

step "Terminating gracefully"
started="$SECONDS"
docker stop --timeout "$STOP_TIMEOUT" "$CONTAINER" >/dev/null
elapsed=$((SECONDS - started))
exit_code="$(docker inspect --format '{{.State.ExitCode}}' "$CONTAINER")"
[ "$exit_code" -eq 0 ] || fail "container exited with code $exit_code after SIGTERM, want 0"
[ "$elapsed" -lt "$STOP_TIMEOUT" ] || fail "container needed ${elapsed}s to stop, want less than ${STOP_TIMEOUT}s"
pass "container exited 0 after SIGTERM in ${elapsed}s"

step "Inspecting the database the container left behind"
# The data directory is owner-only and the container runs as uid 65532, so the
# host user cannot read it directly — which is the property being checked.
# `docker cp` reads it through the daemon and preserves the modes.
docker cp "$CONTAINER:/data/pacapulse" "$WORKDIR/db" >/dev/null

mode="$(stat -c '%a' "$WORKDIR/db")"
[ "$mode" = "700" ] || fail "data directory mode is $mode, want 700"
pass "the data directory has mode $mode"

for name in pacapulse.db pacapulse.lock; do
  [ -f "$WORKDIR/db/$name" ] || fail "$name is missing from the data directory"
  mode="$(stat -c '%a' "$WORKDIR/db/$name")"
  case "$mode" in
    ?00) pass "$name has mode $mode: no group or world access" ;;
    *) fail "$name mode is $mode, want no group or world access" ;;
  esac
done

# A clean shutdown checkpoints and removes the write-ahead log, so its absence
# is evidence that the database was closed rather than merely abandoned.
for name in pacapulse.db-wal pacapulse.db-shm; do
  if [ -e "$WORKDIR/db/$name" ]; then
    fail "$name survived shutdown; the database was not closed cleanly"
  fi
done
pass "shutdown checkpointed and removed the write-ahead log"

# grep keeps these checks free of a SQLite dependency on the host: the file
# starts with SQLite's header and contains the migration ledger's DDL.
head -c 16 "$WORKDIR/db/pacapulse.db" | grep -aq 'SQLite format 3' \
  || fail "the data directory does not contain a SQLite database"
grep -aq 'schema_migrations' "$WORKDIR/db/pacapulse.db" \
  || fail "the database does not contain the migration ledger"
pass "the volume holds a migrated SQLite database"

printf '\nSmoke test passed for %s\n' "$IMAGE"
