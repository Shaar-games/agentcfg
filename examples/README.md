# Configuration examples

These files are samples, not active configuration. The canonical JSON samples have a `.example` suffix; the plugin and standalone skill live outside the user-level `~/.agentmux/`. Merely reading or validating them starts no subprocess and contacts no remote service.

## Chrome DevTools and Context7

Review `canonical/mcp.json.example` and `canonical/agentmux.json.example`. To use them, merge their contents into `~/.agentmux/mcp.json` and `~/.agentmux/agentmux.json` (without the `.example` suffix), after backing up existing files. AgentMux then synchronizes those sources into the selected harnesses' native configuration files. Do not replace existing canonical files wholesale.

The Chrome entry uses the real `node` executable with a JavaScript entry point, so it does not require an npm shell shim. Its explicit `"cwd": "./.agentmux"` resolves from the user home and makes relative arguments point into `~/.agentmux/node_modules/`. Without an explicit `cwd`, canonical servers run from the home directory. To run this example, install a compatible package under `~/.agentmux/node_modules/` yourself or replace the script argument with the absolute JavaScript entry point of your installation. Verify the entry point against [the upstream package manifest](https://github.com/ChromeDevTools/chrome-devtools-mcp/blob/main/package.json) for the version you pin. This example installs nothing.

The upstream alternative is `npx -y chrome-devtools-mcp@latest`, with `command: "npx"` and separate arguments. On Windows, npm may expose a `.cmd` shim; AgentMux supports ordinary wrappers but rejects unsafe batch arguments as described in [transport limitations](../docs/transports.md). The supplied direct-Node form does not depend on a batch wrapper and passes the script and arguments directly to Node.

Context7 uses its documented remote endpoint. No credentials are included. Service availability, authentication and rate limits are checked when a client actually connects, not during configuration loading.

Sources: [Chrome DevTools MCP](https://github.com/ChromeDevTools/chrome-devtools-mcp) and [Context7](https://github.com/upstash/context7).

## Portable plugin

`plugins/docs-helper` is an Agent Plugins 1.0.0 package. Copy that folder to **`~/.agentmux/plugins/docs-helper`** to discover its `docs-helper.context7` server and `docs-helper.documentation-check` skill.

The plugin contains only portable fields. The separate `canonical/agentmux.json.example` demonstrates harness filters and sharing overrides for those qualified names. If you copy settings without the plugin, unmatched override warnings are expected; remove those overrides when you do not want the plugin.

## Standalone skill

Copy `skills/quick-review` to **`~/.agentmux/skills/quick-review`** to discover the standalone `quick-review` skill. Its frontmatter name matches the final folder name.

For supported fields, failure boundaries and path rules, see [configuration documentation](../docs/configuration.md).
