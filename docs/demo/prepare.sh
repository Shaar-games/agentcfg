#!/usr/bin/env bash
set -euo pipefail

# Prepare a deliberately isolated user home inside the CI checkout.
# The fixtures represent community projects but contain no downloaded code,
# secrets, live MCP sessions or instructions to execute.
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
demo="$repo/.demo"
if [[ "$demo" != "$repo/.demo" || ! -d "$repo/.git" ]]; then
  echo "Refusing to prepare the demo outside its repository" >&2
  exit 1
fi
mkdir -p "$demo/home/.agentcfg/skills" "$demo/bin"
cp "$repo/docs/demo/fixtures/agentcfg.json" "$demo/home/.agentcfg/agentcfg.json"
cp "$repo/docs/demo/fixtures/mcp.json" "$demo/home/.agentcfg/mcp.json"
cp -R "$repo/docs/demo/fixtures/skills/." "$demo/home/.agentcfg/skills/"

# VHS runs in a separate container and needs to read its isolated home.
# The setup never uses or inspects the invoking user's $HOME.
chmod -R a+rX "$demo"
