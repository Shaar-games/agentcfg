#!/bin/sh
set -eu

repo=https://github.com/Shaar-games/agentmux.git
dest="${AGENTMUX_INSTALL_DIR:-${HOME}/.local/share/agentmux}"
bindir="${HOME}/.local/bin"
binary="$bindir/agentmux"
if [ -e "$dest" ]; then
  echo "Destination already exists: $dest (refusing to overwrite)" >&2
  exit 1
fi
if [ -e "$binary" ] || [ -L "$binary" ]; then
  echo "Executable already exists: $binary (refusing to overwrite)" >&2
  exit 1
fi
command -v git >/dev/null 2>&1 || { echo 'git is required' >&2; exit 1; }
command -v mise >/dev/null 2>&1 || { echo 'mise is required: https://mise.jdx.dev/' >&2; exit 1; }
mkdir -p "$(dirname "$dest")"
git clone --depth 1 "$repo" "$dest"
cd "$dest"
mise trust
mise install
mise run build
mkdir -p "$bindir"
ln -s "$dest/bin/agentmux" "$binary"
printf 'Installed: %s\n' "$binary"
printf 'Add %s to PATH if needed.\n' "$bindir"
