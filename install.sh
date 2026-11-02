#!/usr/bin/env bash
# Install a verified precompiled GitHub Release. No local compilation.
set -euo pipefail
umask 077
repo="https://github.com/Shaar-games/agentmux"
version="${AGENTMUX_VERSION:-latest}"
if [[ "$version" == latest ]]; then
  base="$repo/releases/latest/download"
elif [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-+._a-zA-Z0-9]*)?$ ]]; then
  base="$repo/releases/download/$version"
else
  printf 'Invalid AGENTMUX_VERSION: %s\n' "$version" >&2
  exit 1
fi
# Override only for testing against an independently hosted release mirror.
base="${AGENTMUX_RELEASE_BASE_URL:-$base}"
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

bindir="${AGENTMUX_BIN_DIR:-$HOME/.local/bin}"
binary="$bindir/agentmux"
if [[ -e "$binary" || -L "$binary" ]]; then
  printf 'Refusing to overwrite existing executable: %s\n' "$binary" >&2
  exit 1
fi
tmp="$(mktemp -d "${TMPDIR:-/tmp}/agentmux-install.XXXXXXXX")"
stage=""
cleanup() {
  if [[ -n "$stage" ]]; then rm -f "$stage"; fi
  rm -rf "$tmp"
}
trap cleanup EXIT
asset="agentmux_${os}_${arch}.tar.gz"
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
tar -xzf "$tmp/$asset" -C "$tmp" agentmux
[[ -f "$tmp/agentmux" && ! -L "$tmp/agentmux" ]] || { echo 'Archive does not contain a regular agentmux executable.' >&2; exit 1; }
mkdir -p "$bindir"
stage="$(mktemp "$bindir/.agentmux-install.XXXXXXXX")"
cp "$tmp/agentmux" "$stage"
chmod 755 "$stage"
# Hardlink creation is exclusive and never overwrites an existing destination.
ln "$stage" "$binary" || { echo 'Cannot install: destination already exists.' >&2; exit 1; }
rm -f "$stage"
stage=""
if ! "$binary" version >/dev/null 2>&1; then
  rm -f "$binary"
  echo 'Binary smoke test failed; installation rolled back.' >&2
  exit 1
fi
printf 'Installed %s\nAdd %s to PATH if necessary.\n' "$binary" "$bindir"
