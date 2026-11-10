#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/test-docker.sh [--skip-npx] [--skip-race]

Build and run the test suite in a disposable Linux container with Go and Node.js.
No host home, MCP configuration, Docker socket or source directory is mounted.

The default run includes go test ./..., go vet ./..., the real npx MCP test,
and race detection for all packages.
--skip-npx skips the npm registry integration; image/dependency downloads remain.
--skip-race skips the additional race detector run.

Optional image versions: AGENTCFG_GO_VERSION, AGENTCFG_NODE_VERSION.
Run from Linux or WSL with a working Docker Engine connection.
EOF
}

test_npx=1
test_race=1
for option in "$@"; do
  case "$option" in
    --skip-npx) test_npx=0 ;;
    --skip-race) test_race=0 ;;
    --help|-h) usage; exit 0 ;;
    *) printf 'Unknown option: %s\n' "$option" >&2; usage >&2; exit 2 ;;
  esac
done

repository=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
if ! command -v docker >/dev/null 2>&1; then
  printf 'Docker CLI is required. Run this script from Linux or WSL with Docker available.\n' >&2
  exit 1
fi
if ! docker version --format '{{.Server.Version}}' >/dev/null; then
  printf 'Docker Engine is unavailable; no container was started.\n' >&2
  exit 1
fi

run_id="$(date -u +%Y%m%d%H%M%S)-$$"
image="agentcfg-mcp-audit:$run_id"
container="agentcfg-mcp-audit-$run_id"
cleanup() {
  result=$?
  trap - EXIT INT TERM
  docker container rm --force "$container" >/dev/null 2>&1 || true
  docker image rm "$image" >/dev/null 2>&1 || true
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Send only the sources and fixtures required by the tests. In particular,
# neither .git nor any project-local/home configuration enters the build context.
tar -C "$repository" -cf - \
  test/integration go.mod go.sum install.sh install.ps1 cmd internal examples |
  docker build --pull --tag "$image" --file test/integration/Dockerfile \
    --build-arg "GO_VERSION=${AGENTCFG_GO_VERSION:-1.26.10}" \
    --build-arg "NODE_VERSION=${AGENTCFG_NODE_VERSION:-24.22.1}" -

docker run --rm --name "$container" --cap-drop=ALL \
  --security-opt=no-new-privileges \
  --tmpfs /tmp:rw,nosuid,nodev \
  --env "AGENTCFG_TEST_NPX=$test_npx" \
  --env "AGENTCFG_TEST_RACE=$test_race" \
  "$image"
