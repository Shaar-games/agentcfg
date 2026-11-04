package bootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agentcfg/internal/syncer"
)

func TestAdoptionHashesAreCapturedDuringDiscovery(t *testing.T) {
	home := t.TempDir()
	native := put(t, home, ".cursor/mcp.json", "// native JSONC\n"+`{"mcpServers":{"fixture":{"command":"node"}}}`)
	put(t, home, ".cursor/skills/review/SKILL.md", skill("review"))
	put(t, home, ".cursor/skills/review/assets/data.bin", string([]byte{0, 1, 2, 255}))
	if err := os.MkdirAll(filepath.Join(home, ".cursor", "skills", "review", "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	plan, err := Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Servers) != 1 || len(plan.Skills) != 1 {
		t.Fatalf("%+v", plan)
	}
	raw, _ := os.ReadFile(native)
	digest := sha256.Sum256(raw)
	got, ok := plan.SourceSHA256(native)
	if !ok || got != hex.EncodeToString(digest[:]) {
		t.Fatal("native hash must include original comments/format")
	}
	if plan.Servers[0].CanonicalSHA256 != syncer.ServerSHA256(plan.Servers[0].Server) {
		t.Fatal("canonical hash differs")
	}
	tree, err := syncer.SkillTreeSHA256(plan.Skills[0].Source)
	if err != nil || tree != plan.Skills[0].TreeSHA256 {
		t.Fatalf("tree hash differs: %s vs %s, %v", tree, plan.Skills[0].TreeSHA256, err)
	}
	put(t, home, ".cursor/mcp.json", `{"mcpServers":{"fixture":{"command":"changed"}}}`)
	after, ok := plan.SourceSHA256(native)
	if !ok || after != got {
		t.Fatal("source proof was recomputed after discovery")
	}
	if _, ok := plan.SourceSHA256(filepath.Join(home, "unknown")); ok {
		t.Fatal("unknown path has a proof")
	}
}

func TestMergePreservesOriginsAndAllCollisionGroups(t *testing.T) {
	home := t.TempDir()
	put(t, home, ".claude.json", `{"mcpServers":{"same":{"command":"node"}}}`)
	put(t, home, ".codex/config.toml", "[mcp_servers.same]\ncommand='other'\n")
	put(t, home, ".cursor/mcp.json", `{"mcpServers":{"same":{"command":"node"}}}`)
	put(t, home, ".claude/skills/review/SKILL.md", skill("review"))
	put(t, home, ".agents/skills/review/SKILL.md", skill("review")+"different\n")
	put(t, home, ".cursor/skills/review/SKILL.md", skill("review"))
	plan, err := Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Servers) != 2 || len(plan.Skills) != 2 {
		t.Fatalf("%+v", plan)
	}
	for _, candidate := range plan.Servers {
		if candidate.Selected || len(candidate.Warnings) == 0 {
			t.Fatalf("server conflict selected: %+v", candidate)
		}
		if candidate.Server.Command == "node" {
			if !reflect.DeepEqual(candidate.Harnesses, []string{"claude-code", "cursor"}) || len(candidate.Origins) != 2 || len(candidate.Sources) != 2 {
				t.Fatalf("lost origins: %+v", candidate)
			}
			for _, origin := range candidate.Origins {
				if origin.Harness == "cursor" && origin.Source != filepath.Join(home, ".cursor", "mcp.json") {
					t.Fatal("crossed origins")
				}
			}
		}
	}
	for _, candidate := range plan.Skills {
		if candidate.Selected {
			t.Fatal("skill collision selected")
		}
	}
	again, err := Discover(home)
	if err != nil || !reflect.DeepEqual(plan, again) {
		t.Fatal("collision inventory is unstable", err)
	}
}

func TestMultipleOpenCodeLocationsRequireSelection(t *testing.T) {
	home := t.TempDir()
	for _, path := range []string{".config/opencode/opencode.json", ".config/opencode/opencode.jsonc", ".opencode/opencode.json"} {
		put(t, home, path, `{"mcp":{"same":{"type":"local","command":["node"]}}}`)
	}
	plan, err := Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Servers) != 1 || plan.Servers[0].Selected || len(plan.Servers[0].Origins) != 3 {
		t.Fatalf("%+v", plan)
	}
}

func TestMalformedAndUnsupportedNativeDefinitions(t *testing.T) {
	tests := []struct {
		name, path, data string
		count            int
	}{
		{"duplicate-root", ".cursor/mcp.json", `{"mcpServers":{},"mcpServers":{"x":{"command":"node"}}}`, 0},
		{"duplicate-name", ".cursor/mcp.json", `{"mcpServers":{"x":{"command":"node"},"x":{"command":"other"}}}`, 0},
		{"bad-sibling", ".cursor/mcp.json", `{"mcpServers":{"good":{"command":"node"},"bad":{"command":"a","command":"b"}}}`, 1},
		{"null-arg", ".cursor/mcp.json", `{"mcpServers":{"x":{"command":"node","args":[null]}}}`, 0},
		{"env-case", ".cursor/mcp.json", `{"mcpServers":{"x":{"command":"node","env":{"Mode":"a","MODE":"b"}}}}`, 0},
		{"header-case", ".cursor/mcp.json", `{"mcpServers":{"x":{"url":"https://example.test","headers":{"X-Mode":"a","x-mode":"b"}}}}`, 0},
		{"remote-http", ".cursor/mcp.json", `{"mcpServers":{"x":{"url":"http://example.test"}}}`, 1},
		{"sse", ".gemini/settings.json", `{"mcpServers":{"x":{"url":"https://example.test/sse"}}}`, 0},
		{"mixed", ".gemini/settings.json", `{"mcpServers":{"x":{"httpUrl":"https://example.test","command":"node"}}}`, 0},
		{"codex-env-auth", ".codex/config.toml", "[mcp_servers.x]\nurl='https://example.test'\nbearer_token_env_var='NO_ENV_READ'\n", 0},
		{"codex-tool-policy", ".codex/config.toml", "[mcp_servers.x]\ncommand='node'\nenabled_tools=['safe']\n", 0},
		{"opencode-policy", ".config/opencode/opencode.json", `{"tools":{"x_*":false},"mcp":{"x":{"type":"local","command":["node"]}}}`, 0},
		{"opencode-argv", ".config/opencode/opencode.json", `{"mcp":{"x":{"type":"local","command":"node --arg"}}}`, 0},
		{"invalid-utf8", ".cursor/mcp.json", string([]byte{255}), 0},
		{"oversize", ".cursor/mcp.json", strings.Repeat(" ", maxConfigBytes+1), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			put(t, home, tt.path, tt.data)
			plan, err := Discover(home)
			if err != nil || len(plan.Servers) != tt.count || len(plan.Warnings) == 0 {
				t.Fatalf("%+v %v", plan, err)
			}
		})
	}
}

func TestPortableSkillsRejectInvalidMetadataAndGeneratedCopies(t *testing.T) {
	invalid := []string{
		"---\nname: review\nname: review\ndescription: test\n---\n",
		"---\nname: Review\ndescription: test\n---\n",
		"---\nname: review\ndescription: 123\n---\n",
		"---\nname: review\ndescription: &ref test\nmetadata:\n  note: *ref\n---\n",
		"---\nname: review\ndescription: test\nmetadata:\n  harness: unknown\n---\n",
		"---\nname: review\ndescription: test\nmetadata:\n  count: 123\n---\n",
	}
	for i, data := range invalid {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			home := t.TempDir()
			put(t, home, ".cursor/skills/review/SKILL.md", data)
			plan, err := Discover(home)
			if err != nil || len(plan.Skills) != 0 || len(plan.Warnings) == 0 {
				t.Fatalf("%+v %v", plan, err)
			}
		})
	}
	home := t.TempDir()
	put(t, home, ".cursor/skills/review/SKILL.md", skill("review"))
	put(t, home, ".cursor/skills/review/.agentcfg-origin.json", "{}")
	put(t, home, ".agents/skills/disabled/SKILL.md", "---\nname: disabled\ndescription: test\nmetadata:\n  harness: none\n---\n")
	plan, err := Discover(home)
	if err != nil || len(plan.Skills) != 1 || plan.Skills[0].Name != "disabled" || plan.Skills[0].Selected {
		t.Fatalf("%+v %v", plan, err)
	}
}

func TestDiscoveryRejectsImplicitHomes(t *testing.T) {
	for _, home := range []string{"", " ", ".", "relative-home"} {
		if _, err := Discover(home); err == nil {
			t.Fatalf("implicit home accepted: %q", home)
		}
	}
}

func TestDiscoveryContainmentAndPortableLinks(t *testing.T) {
	t.Run("config-escape", func(t *testing.T) {
		home, outside := t.TempDir(), t.TempDir()
		put(t, outside, "mcp.json", `{"mcpServers":{"outside":{"command":"SENTINEL_EXTERNAL"}}}`)
		dirLink(t, filepath.Join(home, ".cursor"), outside)
		plan, err := Discover(home)
		if err != nil || len(plan.Servers) != 0 || len(plan.Warnings) == 0 {
			t.Fatalf("%+v %v", plan, err)
		}
	})
	t.Run("nested-link", func(t *testing.T) {
		home, outside := t.TempDir(), t.TempDir()
		put(t, home, ".cursor/skills/review/SKILL.md", skill("review"))
		put(t, outside, "asset.txt", "outside")
		dirLink(t, filepath.Join(home, ".cursor", "skills", "review", "assets"), outside)
		plan, err := Discover(home)
		if err != nil || len(plan.Skills) != 0 || len(plan.Warnings) == 0 {
			t.Fatalf("%+v %v", plan, err)
		}
	})
	t.Run("contained-top-alias", func(t *testing.T) {
		home := t.TempDir()
		source := filepath.Dir(put(t, home, "packs/review/SKILL.md", skill("review")))
		native := filepath.Join(home, ".cursor", "skills", "review")
		if err := os.MkdirAll(filepath.Dir(native), 0700); err != nil {
			t.Fatal(err)
		}
		dirLink(t, native, source)
		plan, err := Discover(home)
		if err != nil || len(plan.Skills) != 1 {
			t.Fatalf("%+v %v", plan, err)
		}
		if plan.Skills[0].Source != source || plan.Skills[0].Origins[0].Source != native {
			t.Fatalf("alias origin lost: %+v", plan.Skills[0])
		}
		if err = plan.Revalidate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("canonical-alias", func(t *testing.T) {
		home := t.TempDir()
		source := filepath.Dir(put(t, home, ".agentcfg/skills/review/SKILL.md", skill("review")))
		native := filepath.Join(home, ".cursor", "skills", "review")
		if err := os.MkdirAll(filepath.Dir(native), 0700); err != nil {
			t.Fatal(err)
		}
		dirLink(t, native, source)
		plan, err := Discover(home)
		if err != nil || len(plan.Skills) != 0 {
			t.Fatalf("%+v %v", plan, err)
		}
	})
}
