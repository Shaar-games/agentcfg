#!/usr/bin/env bash
# Install a verified precompiled GitHub Release. No local compilation.
set -euo pipefail
umask 077
repo="https://github.com/Shaar-games/agentcfg"
version="${AGENTCFG_VERSION:-latest}"
if [[ "$version" == latest ]]; then
  base="$repo/releases/latest/download"
elif [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-+._a-zA-Z0-9]*)?$ ]]; then
  base="$repo/releases/download/$version"
else
  printf 'Invalid AGENTCFG_VERSION: %s\n' "$version" >&2
  exit 1
fi
# Override only for testing against an independently hosted release mirror.
base="${AGENTCFG_RELEASE_BASE_URL:-$base}"
base="${base%/}"

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) printf 'Unsupported OS: use install.ps1 on Windows.\n' >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) printf 'Unsupported CPU architecture: %s\n' "$(uname -m)" >&2; exit 1 ;;
esac
for tool in curl tar mktemp; do
  command -v "$tool" >/dev/null 2>&1 || { printf 'Missing %s.\n' "$tool" >&2; exit 1; }
done
if command -v sha256sum >/dev/null 2>&1; then
  checksum() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  checksum() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  printf 'SHA-256 verification requires sha256sum or shasum.\n' >&2
  exit 1
fi

bindir="${AGENTCFG_BIN_DIR:-$HOME/.local/bin}"
binary="$bindir/agentcfg"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/agentcfg-install.XXXXXXXX")"
stage=""
backup=""
cleanup() {
  if [[ -n "$stage" ]]; then rm -f "$stage"; fi
  rm -rf "$tmp"
}
trap cleanup EXIT
asset="agentcfg_${os}_${arch}.tar.gz"
printf 'Downloading precompiled %s (%s)...\n' "$asset" "$version"
curl -fLsS --retry 2 --connect-timeout 15 --max-time 240 -o "$tmp/$asset" "$base/$asset"
curl -fLsS --retry 2 --connect-timeout 15 --max-time 60 -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"
expected=""
while IFS= read -r line || [[ -n "$line" ]]; do
  if [[ "$line" =~ ^([[:xdigit:]]{64})[[:space:]]+\*?(.+)$ && "${BASH_REMATCH[2]}" == "$asset" ]]; then
    [[ -z "$expected" ]] || { echo 'Duplicate checksum entry.' >&2; exit 1; }
    expected="$(printf '%s' "${BASH_REMATCH[1]}" | tr '[:upper:]' '[:lower:]')"
  fi
done < "$tmp/SHA256SUMS"
[[ -n "$expected" ]] || { echo 'Missing checksum entry.' >&2; exit 1; }
actual="$(checksum "$tmp/$asset")"
[[ "$actual" == "$expected" ]] || { echo 'SHA-256 mismatch: refusing installation.' >&2; exit 1; }

# Extract only the expected filename; reject a missing or symlinked executable.
tar -xzf "$tmp/$asset" -C "$tmp" agentcfg
[[ -f "$tmp/agentcfg" && ! -L "$tmp/agentcfg" ]] || { echo 'Archive does not contain a regular agentcfg executable.' >&2; exit 1; }
mkdir -p "$bindir"
stage="$(mktemp "$bindir/.agentcfg-install.XXXXXXXX")"
cp "$tmp/agentcfg" "$stage"
chmod 755 "$stage"
# Verify the staged binary before touching any existing installation.
"$stage" version >/dev/null || { echo 'Staged binary smoke test failed; keeping current installation.' >&2; exit 1; }
if [[ -e "$binary" || -L "$binary" ]]; then
  [[ -f "$binary" && ! -L "$binary" ]] || {
    printf 'Existing destination is not a regular executable: %s\n' "$binary" >&2
    exit 1
  }
  backup="$(mktemp "$bindir/.agentcfg-backup.XXXXXXXX")"
  cp -p "$binary" "$backup"
fi
# Stage and destination are on the same filesystem. Unix rename replaces the
# destination atomically; retain a temporary backup for smoke-test rollback.
if ! mv -f "$stage" "$binary"; then
  [[ -z "$backup" ]] || rm -f "$backup"
  echo 'Unable to install verified binary; previous installation retained.' >&2
  exit 1
fi
stage=""
if ! "$binary" version >/dev/null 2>&1; then
  if [[ -n "$backup" ]]; then
    mv -f "$backup" "$binary" || {
      printf 'Rollback failed; previous binary retained at %s\n' "$backup" >&2
      exit 1
    }
    backup=""
  else
    rm -f "$binary"
  fi
  echo 'Binary smoke test failed; installation rolled back.' >&2
  exit 1
fi
if [[ -n "$backup" ]]; then
  rm -f "$backup"
  backup=""
fi
printf 'Installed/updated %s\nAdd %s to PATH if necessary.\n' "$binary" "$bindir"
