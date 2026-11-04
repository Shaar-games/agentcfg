package syncer

import (
	"bytes"
	"os"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

func TestTOMLAdoptionPreservesOtherSettingsAndRemainsIdempotent(t *testing.T) {
	for _, input := range []string{
		"[mcp_servers.echo]\ncommand='node'\nargs=['server.js']\n",
		"# user heading\nmodel='keep'\n[mcp_servers.echo]\ncommand='node'\nargs=[\n# array comment\n'server.js',\n]\n# keep unrelated comment\n[unrelated]\nvalue=42\n",
		"[mcp_servers.echo]\ncommand='node'\nargs=[\n'server.js',\n]\n[mcp_servers.echo.env]\n# keep final comment\n",
		"# user heading\nmodel = 'keep'\n[mcp_servers.echo]\ncommand = 'node'\nargs = ['server.js']\n[mcp_servers.manual]\ncommand='keep'\n[projects.'quoted.path']\ntrust_level='trusted'\n",
		"\ufeffmodel='keep'\r\n[mcp_servers.\"echo\"]\r\ncommand='node'\r\nargs=['server.js']\r\n[unrelated]\r\ntext='''line one\r\n[mcp_servers.fake]\r\nline three'''\r\n",
		"model='keep'\nmcp_servers.echo.command='node'\nmcp_servers.echo.args=['server.js']\n[unrelated]\nvalue=42\n",
		"model='keep'\n[mcp_servers]\necho={command='node',args=['server.js']}\nmanual={command='keep'}\n[unrelated]\nvalue=42\n",
		"mcp_servers={echo={command='node',args=['server.js']},manual={command='keep'}}\nmodel='keep'\n[unrelated]\nvalue=42\n",
	} {
		t.Run(input, func(t *testing.T) {
			f := newAdoptionFixture(t, "codex", false)
			writeTestFile(t, f.target.path, input)
			f.opts.AdoptMCP[0].FileSHA256 = rawDigest([]byte(input))
			plan, err := BuildPlan(f.cfg, f.opts)
			if err != nil {
				t.Fatal(err)
			}
			assertAdoptionBytes(t, f.target.path, []byte(input))
			if err := plan.Apply(); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(f.target.path)
			if err != nil {
				t.Fatal(err)
			}
			var beforeDoc, afterDoc map[string]any
			if err := toml.Unmarshal(bytes.TrimPrefix([]byte(input), []byte{0xef, 0xbb, 0xbf}), &beforeDoc); err != nil {
				t.Fatal(err)
			}
			if err := toml.Unmarshal(bytes.TrimPrefix(after, []byte{0xef, 0xbb, 0xbf}), &afterDoc); err != nil {
				t.Fatal(err)
			}
			beforeServers := beforeDoc["mcp_servers"].(map[string]any)
			afterServers := afterDoc["mcp_servers"].(map[string]any)
			if digest(beforeServers["manual"]) != digest(afterServers["manual"]) {
				t.Fatal("unapproved MCP changed")
			}
			if digest(afterServers["echo"]) != digest(proxyEntry(f.target, f.opts.Binary, f.cfg.Root, "echo")) {
				t.Fatal("native MCP is not the broker proxy")
			}
			delete(beforeDoc, "mcp_servers")
			delete(afterDoc, "mcp_servers")
			if digest(beforeDoc) != digest(afterDoc) {
				t.Fatal("unrelated settings changed")
			}
			f.opts.AdoptExisting, f.opts.AdoptMCP = false, nil
			next, err := BuildPlan(f.cfg, f.opts)
			if err != nil || len(next.Changes) != 0 {
				t.Fatalf("not idempotent: %v %+v", err, next)
			}
		})
	}
}

func TestTOMLAdoptionRejectsPostPreviewEdits(t *testing.T) {
	f := newAdoptionFixture(t, "codex", false)
	plan, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	changed := append(append([]byte(nil), f.before...), []byte("# concurrent user edit\n")...)
	writeTestFile(t, f.target.path, string(changed))
	if err := plan.Apply(); err == nil {
		t.Fatal("concurrent TOML edit was overwritten")
	}
	assertAdoptionBytes(t, f.target.path, changed)
	assertNoAdoptionManifest(t, f.opts.Home)
}
