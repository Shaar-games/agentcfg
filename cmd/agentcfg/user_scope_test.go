package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeScopeFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func projectSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			result[rel] = "directory"
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[rel] = fmt.Sprintf("%x", sha256.Sum256(content))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// Run from a normal project without a checked-in .agentcfg directory. CLI
// inventory, validation and synchronization must use the temporary user home
// only, including when a project has its own unrelated harness files.
func TestGlobalCLIUsesHomeWithoutProjectagentcfg(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	writeScopeFixture(t, filepath.Join(project, "AGENTS.md"), "# Project instructions must remain untouched\n")
	writeScopeFixture(t, filepath.Join(project, ".agents", "skills", "personal", "SKILL.md"), "project skill\n")
	writeScopeFixture(t, filepath.Join(project, ".cursor", "mcp.json"), `{"mcpServers":{"personal":{"command":"project-owned"}}}`)
	t.Chdir(project)
	before := projectSnapshot(t, project)
	var output, diagnostics bytes.Buffer
	runCLI := func(args ...string) string {
		t.Helper()
		output.Reset()
		diagnostics.Reset()
		if err := run(context.Background(), args, strings.NewReader(""), &output, &diagnostics); err != nil {
			t.Fatalf("agentcfg %q: %v; stderr=%s", args, err, diagnostics.String())
		}
		return output.String()
	}
	runCLI("init", "--harnesses", "codex,cursor,antigravity", "--yes")
	canonical := filepath.Join(home, ".agentcfg")
	writeScopeFixture(t, filepath.Join(canonical, "mcp.json"), `{"mcpServers":{"local":{"type":"stdio","command":"go"}}}`)
	writeScopeFixture(t, filepath.Join(canonical, "skills", "review", "SKILL.md"), "---\nname: review\ndescription: Inspect code\nmetadata:\n  harness: codex,cursor,antigravity\n---\n# Preserve these bytes\n")
	plugin := filepath.Join(canonical, "plugins", "docs-plugin")
	writeScopeFixture(t, filepath.Join(plugin, "plugin.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"docs-plugin"}`)
	writeScopeFixture(t, filepath.Join(plugin, "mcp.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json","mcpServers":{"remote":{"type":"streamable-http","url":"https://example.invalid/mcp"}}}`)
	writeScopeFixture(t, filepath.Join(plugin, "skills", "assist", "SKILL.md"), "---\nname: assist\ndescription: Portable plugin skill\n---\n# Plugin instructions\n")
	writeScopeFixture(t, filepath.Join(home, ".cursor", "mcp.json"), `{"mcpServers":{"personal":{"command":"home-owned"}},"theme":"keep"}`)
	if got := runCLI("list"); !strings.Contains(got, "docs-plugin.remote") || !strings.Contains(got, "docs-plugin.assist") || !strings.Contains(got, "review") {
		t.Fatalf("missing canonical MCP/plugin/skill inventory: %s", got)
	}
	runCLI("validate")
	if got := runCLI("sync"); !strings.Contains(got, "Preview:") {
		t.Fatalf("expected read-only synchronization preview: %s", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("preview wrote native configuration: %v", err)
	}
	if projectAfter := projectSnapshot(t, project); !sameProjectSnapshot(before, projectAfter) {
		t.Fatalf("preview modified project: before=%v after=%v", before, projectAfter)
	}
	runCLI("sync", "--apply")
	for _, target := range []string{
		filepath.Join(home, ".cursor", "skills", "review", "SKILL.md"),
		filepath.Join(home, ".agents", "skills", "review", "SKILL.md"),
		filepath.Join(home, ".gemini", "config", "skills", "review", "SKILL.md"),
	} {
		source, err := os.ReadFile(filepath.Join(canonical, "skills", "review", "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		actual, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(source, actual) {
			t.Fatalf("skill copy/symlink altered content at %s: %v", target, err)
		}
	}
	writeScopeFixture(t, filepath.Join(canonical, "agentcfg.json"), `{"harnesses":[]}`)
	runCLI("sync", "--apply")
	content, err := os.ReadFile(filepath.Join(home, ".cursor", "mcp.json"))
	if err != nil || !strings.Contains(string(content), "home-owned") || strings.Contains(string(content), "docs-plugin.remote") || strings.Contains(string(content), `"local"`) {
		t.Fatalf("managed entries removed incorrectly or home-owned entry lost: %s (%v)", content, err)
	}
	for _, forbidden := range [][]string{{"sync", "--scope", "project"}, {"sync", "--root", project}, {"init", "--root", project}} {
		if err := run(context.Background(), forbidden, strings.NewReader(""), &output, &diagnostics); err == nil {
			t.Fatalf("deprecated project-scope flag accepted: %q", forbidden)
		}
	}
	if projectAfter := projectSnapshot(t, project); !sameProjectSnapshot(before, projectAfter) {
		t.Fatalf("global CLI modified project: before=%v after=%v", before, projectAfter)
	}
	if _, err := os.Stat(filepath.Join(project, ".agentcfg")); !os.IsNotExist(err) {
		t.Fatalf("CLI created project .agentcfg: %v", err)
	}
}

func sameProjectSnapshot(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for path, digest := range a {
		if b[path] != digest {
			return false
		}
	}
	return true
}
