#!/bin/sh
set -eu

repo=https://github.com/Shaar-games/agentmux.git
dest="${AGENTMUX_INSTALL_DIR:-${HOME}/.local/share/agentmux}"
if [ -e "$dest" ]; then
  echo "Destination already exists: $dest (refusing to overwrite)" >&2
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
printf 'Installed: %s/bin/agentmux\n' "$dest"
printf 'Add %s/bin to PATH, or invoke the executable using its absolute path.\n' "$dest"
