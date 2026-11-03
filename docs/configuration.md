# Configuration and portable plugins

`config.Load(root)` reads canonical sources under `<root>/.agentmux/`. The public CLI uses the user's home directory as `root` and synchronizes definitions through native harness configuration adapters. Loading returns resolved servers, discovered skills, settings and warnings without executing processes, looking up executables, contacting networks, creating directories or writing harness configuration.

## Sources

| Location relative to root | Purpose |
| --- | --- |
| `.agentmux/mcp.json` | Explicit canonical MCP servers and optional agentmux fields |
| `.agentmux/agentmux.json` | Selected harnesses, runtime durations and named overrides |
| `.agentmux/skills/<skill>/SKILL.md` | Standalone skills |
| `.agentmux/plugins/<folder>/plugin.json` | Required portable plugin manifest |
| `.agentmux/plugins/<folder>/mcp.json` | Optional portable MCP component |
| `.agentmux/plugins/<folder>/skills/<skill>/SKILL.md` | Optional portable skills |

Only immediate plugin directories and immediate skill directories are discovered. The manifest name, rather than the installation folder name, supplies a plugin's namespace. Missing optional sources are valid. The source root itself must exist and be a directory.

## Canonical MCP servers

~~~json
{
  "mcpServers": {
    "local-tools": {
      "type": "stdio",
      "command": "node",
      "args": ["./tools/server.mjs"],
      "env": {"LOG_LEVEL": "info"},
      "harnesses": ["claude-code", "codex"],
      "sharing": "shared"
    },
    "documentation": {
      "type": "streamable-http",
      "url": "https://example.com/mcp",
      "sharing": "isolated"
    }
  }
}
~~~

The root object accepts `mcpServers` and an optional `$schema`. If present, the schema identifier must be the supported MCP identifier shown below. Canonical server objects can add `harnesses` and `sharing`; those additions are agentmux configuration, not part of the portable plugin schema.

| Transport | Required fields | Optional fields |
| --- | --- | --- |
| `stdio` | `command` | `args`, `env`, `cwd` |
| `streamable-http` | `url` | `headers` |

An omitted canonical `type` is inferred from `command` or, if no command is supplied, `url`. A plugin must declare its type explicitly. Fields belonging to another transport and other unknown fields invalidate that entry. Legacy `sse` entries produce a warning and are skipped; this does not prohibit SSE responses belonging to Streamable HTTP.

`command` is an executable token and `args` is an array of separate strings. The loader does not construct a shell command. Bare executables are left for the launcher to resolve. Canonical path commands may be absolute or relative to the source root and must resolve to regular files. Canonical `cwd` defaults to the source root; an explicit relative value resolves from that root and must already be a directory. Machine-specific absolute paths are allowed in canonical configuration.

Environment names must be nonempty, contain neither NUL nor `=`, and be unique ignoring case. Argument and environment values cannot contain NUL bytes. These checks do not interpret arguments as filesystem paths.

## Harness selection and overrides

The default settings are:

~~~json
{
  "harnesses": ["claude-code", "codex", "cursor", "opencode", "gemini"],
  "runtime": {
    "idleTimeout": "5m",
    "requestTimeout": "5m"
  },
  "serverOverrides": {}
}
~~~

`antigravity` is also accepted and targets `~/.gemini/config/mcp_config.json` and `~/.gemini/config/skills/`, the Google Antigravity personal layout. `vscode` is recognized when loading legacy settings but user-scope VS Code synchronization remains unsupported by the global-only CLI: `agentmux init --harnesses vscode` refuses to create that unsupported selection. Duplicate harness names are removed while retaining their order. Unknown names are errors.

Runtime durations use Go duration syntax, such as `30s` or `5m`, and must be positive. An omitted field retains its default.

The global `harnesses` list selects sync destinations. An explicit empty global list selects none. A server or skill filter narrows those destinations. An omitted filter allows all selected harnesses. For skills, `metadata.harness: none` represents an explicitly empty selection. Direct CLI connections with no harness name are allowed by `config.Allows`; these filters are routing preferences, not authorization controls.

Use fully qualified plugin IDs in overrides:

~~~json
{
  "harnesses": ["claude-code", "codex", "antigravity"],
  "serverOverrides": {
    "docs-helper.context7": {
      "harnesses": ["codex", "claude-code"],
      "sharing": "isolated"
    }
  }
}
~~~

Server overrides replace only fields explicitly present. For example, `"harnesses": []` clears a narrower filter. `sharing` accepts exactly `shared` or `isolated`; an empty string is invalid. Skills use `metadata.harness` in their own SKILL.md, not settings overrides.

`shared` is the server default and requests a shared upstream instance. `isolated` requests a separate upstream connection for each client. Choose the latter when an MCP relies on private session state or client callbacks; see the runtime documentation for the supported shared-session operations.

Harness-native discovery may still overlap the generated skill destinations. For example, a harness that reads `~/.agents/skills/` directly can see skills installed there for Codex, even if its own agentmux filter excludes them. The canonical sources under `~/.agentmux/skills/` are deliberately separate from the native discovery directory. A distribution filter is not an isolation or authorization boundary.

## Portable manifests

A minimal `plugin.json` is:

~~~json
{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "docs-helper"
}
~~~

The required name uses 1–64 ASCII lowercase letters, numbers, dots or hyphens, begins and ends with an alphanumeric character, and contains neither `--` nor `..`. Optional metadata fields are `version`, `description`, `author`, `homepage`, `repository`, `license`, `keywords` and `extensions`.

Metadata is checked by its specified JSON type. Version, URL, email and license strings are not rejected for lacking a particular external format. An author object permits only string fields `name`, `email` and `url`; keywords must be strings.

The author schema is closed, but the normative client rules specify two recovery cases: **report and ignore unknown top-level manifest fields**, and **report and ignore non-object extensions**. Other manifest violations reject that plugin. Agentmux does not implement extension namespaces, so their values are ignored without assigning semantics. Unknown manifest fields never override the fixed component locations. These recovery rules follow [loading and discovery](https://agent-plugins.org/client-implementers/loading-and-discovery) and [specification §§5.2 and 8.1](https://agent-plugins.org/specification).

## Portable MCP components

~~~json
{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
  "mcpServers": {
    "context7": {
      "type": "streamable-http",
      "url": "https://mcp.context7.com/mcp"
    }
  }
}
~~~

Both `$schema` and `mcpServers` are required, and no other top-level fields are permitted. Each server uses the closed portable transport variant. Thus `harnesses`, `sharing`, `disabled` and inline client options invalidate a portable server entry; put agentmux preferences in the external overrides file.

For this example the resolved identifier is `docs-helper.context7`. A plugin's skill `documentation-check` becomes `docs-helper.documentation-check` internally. Its original skill name and source frontmatter remain unchanged.

Agentmux recognizes Agent Plugins 1.0.0 locally. It never fetches a declared schema while loading. Unsupported or mismatched component schema identifiers disable the relevant plugin/component.

## Plugin paths and variables

The plugin root is resolved through the filesystem. Plugin root symlinks are allowed; all package locations are then bounded by that resolved root. Manifest, MCP, command, working-directory and skill paths are checked after resolving symlinks and Windows junctions/reparse paths.

On Windows, the shared path resolver obtains the final path through a metadata handle and [GetFinalPathNameByHandle](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-getfinalpathnamebyhandlew). This also resolves directory junctions that the standard Go path helper can leave at their apparent location.

For plugin stdio servers:

| Field | Interpretation |
| --- | --- |
| `command` | Bare executable name, or a package path beginning `./` |
| omitted `cwd` | Resolved plugin root |
| `cwd: "./work"` | Existing directory inside the plugin |
| `cwd: "${PLUGIN_ROOT}/work"` | Existing directory inside the plugin |
| `cwd: "${PLUGIN_DATA}"` or a child | Dedicated data directory; may be absent during load |
| `args` and environment values | Opaque strings with the two supported substitutions |

Portable paths use forward slashes; explicit backslashes and drive/stream colons in package-relative fields are rejected. Traversal or a link resolving outside the permitted root invalidates that entry. A package command must resolve to a regular file.

`${PLUGIN_ROOT}` and `${PLUGIN_DATA}` are replaced textually, in one non-recursive pass, in arguments, environment values and working directories. Unknown placeholders stay literal. Replacement never applies to commands, environment keys, URLs or headers, and does not expand arbitrary host environment variables.

The launcher receives reserved `PLUGIN_ROOT` and `PLUGIN_DATA` environment variables. Plugin-defined replacements, including case variants, are rejected. Data is assigned to:

~~~text
<user-home>/.agentmux/data/plugins/<manifest-name>
~~~

That location remains stable across updates using the same manifest name. Existing data paths must be directories, remain within the source root, and must not alias another plugin's data directory. The loader computes missing paths without creating them. The process launcher is responsible for creating the data directory and a selected data-rooted working directory immediately before launch.

Containment protects package paths read during this load. It does not sandbox a subprocess or freeze a filesystem against later changes. Arguments and environment values remain opaque even when they look like paths.

## Remote URLs and headers

URLs must be absolute HTTP(S), have a valid host/port, and contain no user information or fragment. HTTP is permitted only for exactly `localhost` or a loopback IP literal; other endpoints require HTTPS. IPv6 literals use brackets. Empty/invalid ports, malformed authorities and unsupported URL schemes invalidate only their entry.

Headers are literal string maps with valid HTTP names and values. Duplicate names ignoring case and control-character injection are rejected. Portable headers are visible package data and should not contain credentials. Agent Plugins 1.0.0 defines no portable OAuth or credential-reference fields; the loader does not perform authentication. See the [MCP component specification](https://agent-plugins.org/plugin-authors/mcp-servers).

## Skills

Both standalone and plugin skills require a regular `SKILL.md` with YAML frontmatter:

~~~markdown
---
name: documentation-check
description: Check official documentation when a task depends on a library API.
metadata:
  harness: codex,claude-code,cursor
---
Read the relevant documentation and cite the version used.
~~~

The name must match its discovered directory basename, use 1–64 lowercase Unicode letters, digits or hyphens, and have no leading, trailing or doubled hyphen. The description must be a nonempty string of at most 1,024 characters. The optional `license` and `allowed-tools` fields are strings; `compatibility` is a string of 1–500 characters; `metadata` is a string-to-string map. Additional frontmatter fields are parsed safely but receive no agentmux semantics.

BOM and CRLF input are supported. Duplicate YAML keys, aliases, anchors, custom tags, malformed types and missing delimiters reject only the affected skill. No skill instructions or scripts are executed by loading.

The full skill tree is checked for escaped links, cycles and special files because sync may copy that tree. In-root aliases retain the discovered basename for installation, even when the filesystem target has a different name. A sync implementation may impose additional restrictions on nested symlinks.

These are explicit implementation limits: configuration/skill files are limited to 1 MiB, frontmatter to 64 KiB, YAML to 32 levels and 4,096 nodes, and one skill tree to 10,000 visited entries.

Standalone directories containing the reserved `.agentmux-origin.json` marker are generated copies and are skipped. Standalone links into the plugin source tree or a loaded plugin root are also skipped to avoid rediscovering materialized plugin skills. A rejected plugin cannot suppress unrelated standalone skills.

## Errors and precedence

| Failure | Result |
| --- | --- |
| Invalid source root, settings, or top-level canonical MCP document | `Load` returns an error |
| Invalid canonical server | Warning; other entries continue |
| Invalid/missing plugin manifest | Warning; that plugin is skipped |
| Invalid plugin MCP document or fixed component path | Warning; that component is skipped |
| Invalid plugin server or individual skill | Warning; its siblings continue |
| Missing optional plugin component | No error |
| Duplicate qualified server ID | Warning; first valid entry wins |
| Duplicate plugin manifest name | Warning; first valid plugin directory wins |

Explicit canonical servers load first and take precedence over colliding plugin IDs. Plugin directory names and server keys are processed lexically. Skills are returned sorted by qualified name. Duplicate keys are rejected at their owning JSON object boundary; semantic errors inside one server do not invalidate its siblings.

The loader does not install packages, execute client extensions, synchronize native configurations in either direction, or infer secrets. Bidirectional configuration import is deferred.

## Examples and references

The checked-in [examples](../examples/README.md) are inactive until copied deliberately to the canonical locations. They include Chrome DevTools plus Context7, a portable plugin, and a standalone skill.

Primary format references:

- [Agent Plugins specification](https://agent-plugins.org/specification)
- [Manifest reference](https://agent-plugins.org/plugin-authors/manifest)
- [MCP server reference](https://agent-plugins.org/plugin-authors/mcp-servers)
- [Loading and discovery](https://agent-plugins.org/client-implementers/loading-and-discovery)
- [Plugin skills](https://agent-plugins.org/plugin-authors/skills)
- [Agent Skills specification](https://agentskills.io/specification)
- [Manifest schema](https://agent-plugins.org/schemas/1.0.0/plugin.schema.json)
- [MCP schema](https://agent-plugins.org/schemas/1.0.0/mcp.schema.json)
