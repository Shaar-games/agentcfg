# Recorded CLI demos

The two `.tape` files record the **real agentcfg executable** with [VHS](https://github.com/charmbracelet/vhs). GitHub Actions rebuilds their GIFs when the CLI or demo scripts change and commits only the generated images.

The fixtures are **synthetic, inert samples**, referencing the names and public shapes of community projects:

- [Context7 MCP](https://github.com/upstash/context7) — HTTP endpoint name and URL
- [Playwright MCP](https://github.com/microsoft/playwright-mcp) — stdio command name and arguments
- [Vercel React Best Practices and Web Design Guidelines skills](https://github.com/vercel-labs/agent-skills) — names and minimal, original placeholder frontmatter

They are **not installed from these projects**. VHS uses an isolated `.demo/home` and exercises `agentcfg config`, `agentcfg list` and `agentcfg mcps --plain` without starting MCP processes, executing skill instructions, reaching external MCP services, or reading any real user configuration.

To reproduce on Linux with Go and Docker:

```bash
go build -trimpath -o .demo/bin/agentcfg ./cmd/agentcfg
bash docs/demo/prepare.sh
docker run --rm --network none -v "$PWD:/vhs" ghcr.io/charmbracelet/vhs docs/demo/agentcfg-config.tape
docker run --rm --network none -v "$PWD:/vhs" ghcr.io/charmbracelet/vhs docs/demo/agentcfg-list.tape
```

The workflow uploads both GIFs as a CI artifact and refreshes `docs/demo/*.gif` on `main`. Publishing a generated image does not modify the demo script or application sources.
