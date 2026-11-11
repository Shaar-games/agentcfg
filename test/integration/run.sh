#!/usr/bin/env bash
set -euo pipefail

# This script runs inside the disposable image. Every test cache/profile and
# temporary file stays below one temporary directory; no host paths are mounted.
audit_root=$(mktemp -d /tmp/agentcfg-tests.XXXXXX)
cleanup() {
  result=$?
  trap - EXIT
  # Go's module cache deliberately contains read-only directories.
  chmod -R u+w -- "$audit_root"
  rm -rf -- "$audit_root"
  exit "$result"
}
trap cleanup EXIT
mkdir -p "$audit_root"/{home,cache,config,data,state,appdata,localappdata,tmp,gocache,gopath}
# Reuse the modules downloaded while building the image without letting tests
# update that image cache or requiring a second network download.
cp -a "$(go env GOMODCACHE)" "$audit_root/gomod"

env HOME="$audit_root/home" USERPROFILE="$audit_root/home" \
  XDG_CACHE_HOME="$audit_root/cache" XDG_CONFIG_HOME="$audit_root/config" \
  XDG_DATA_HOME="$audit_root/data" XDG_STATE_HOME="$audit_root/state" \
  APPDATA="$audit_root/appdata" LOCALAPPDATA="$audit_root/localappdata" \
  TMPDIR="$audit_root/tmp" TMP="$audit_root/tmp" TEMP="$audit_root/tmp" \
  GOCACHE="$audit_root/gocache" GOPATH="$audit_root/gopath" GOMODCACHE="$audit_root/gomod" \
  bash -euo pipefail -c '
    go version
    node --version
    npm --version
    AGENTCFG_TEST_NPX=0 go test ./... -count=1 -timeout=300s
    go vet ./...
    if [[ ${AGENTCFG_TEST_RACE:-1} == 1 ]]; then
      AGENTCFG_TEST_NPX=0 go test -race ./... -count=1 -timeout=300s
    fi
    if [[ ${AGENTCFG_TEST_NPX:-1} == 1 ]]; then
      AGENTCFG_TEST_NPX=1 go test ./internal/upstream ./cmd/agentcfg \
        -run "^Test(NPXFilesystem|CLINPXFilesystem)$" -count=1 -timeout=300s
    fi
  '
