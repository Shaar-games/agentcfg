# Configuration examples

These files are samples, not active configuration. The canonical JSON samples have a `.example` suffix; the plugin and standalone skill live outside `.agentmux`. Merely reading or validating them starts no subprocess and contacts no remote service.

## Chrome DevTools and Context7

Review `canonical/mcp.json.example` and `canonical/agentmux.json.example`. To use them, copy them into the chosen project's `.agentmux` directory with the `.example` suffix removed. Adjust the harness list and paths first.

The Chrome entry uses the real `node` executable with the installed package's JavaScript entry point, so it does not require an npm shell shim. It expects `chrome-devtools-mcp` to have been installed into that project's `node_modules`. The path is based on [the upstream package manifest](https://github.com/ChromeDevTools/chrome-devtools-mcp/blob/main/package.json); verify it for the version you install. The example does not install Node, Chrome or npm packages.

The upstream alternative is `npx -y chrome-devtools-mcp@latest`, with `command: "npx"` and separate arguments. On Windows, npm may expose a `.cmd` shim. The supplied direct-Node form avoids depending on the launcher's constrained support for `.cmd` and `.bat` wrappers and passes the script and arguments directly to Node.

Context7 uses its documented remote endpoint. No credentials are included. Service availability, authentication and rate limits are checked when a client actually connects, not during configuration loading.

Sources: [Chrome DevTools MCP](https://github.com/ChromeDevTools/chrome-devtools-mcp) and [Context7](https://github.com/upstash/context7).

## Portable plugin

`plugins/docs-helper` is an Agent Plugins 1.0.0 package. Copy that folder to `.agentmux/plugins/docs-helper` to discover its `docs-helper.context7` server and `docs-helper.documentation-check` skill.

The plugin contains only portable fields. The separate `canonical/agentmux.json.example` demonstrates harness filters and sharing overrides for those qualified names. If you copy settings without the plugin, unmatched override warnings are expected; remove those overrides when you do not want the plugin.

## Standalone skill

Copy `skills/quick-review` to `.agentmux/skills/quick-review` to discover the standalone `quick-review` skill. Its frontmatter name matches the final folder name.

For supported fields, failure boundaries and path rules, see [configuration documentation](../docs/configuration.md).
