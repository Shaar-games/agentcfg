package syncer

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/config"
	"agentcfg/internal/pathutil"
)

type adoptionFixture struct {
	cfg                         *config.Config
	opts                        Options
	target                      target
	before                      []byte
	canonicalSkill, nativeSkill string
}

func newAdoptionFixture(t *testing.T, harness string, skills bool) adoptionFixture {
	t.Helper()
	// These tests assert canonical plan/manifest keys. Mixed native aliases are
	// exercised separately by the path-alias regression tests.
	home, err := pathutil.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(home, ".agentcfg", "mcp.json"), `{"mcpServers":{"echo":{"command":"node","args":["server.js"]}}}`)
	settings, _ := json.Marshal(map[string]any{"harnesses": []string{harness}})
	writeTestFile(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), string(settings))
	var native target
	for _, candidate := range registry(home, "user") {
		if candidate.name == harness {
			native = candidate
		}
	}
	if harness == "opencode" {
		native.path += "c"
	}
	entry := map[string]any{"command": "node", "args": []string{"server.js"}}
	if harness == "opencode" {
		entry = map[string]any{"type": "local", "command": []string{"node", "server.js"}, "enabled": true}
	}
	document := map[string]any{
		"theme": map[string]any{"name": "untouched", "enabled": true},
		native.key: map[string]any{
			"echo":   entry,
			"manual": map[string]any{"command": "keep-this-command", "args": []string{"untouched"}},
		},
	}
	data, _ := json.MarshalIndent(document, "", "  ")
	data = append([]byte("// Keep this native comment.\n"), data...)
	if harness == "codex" {
		data = []byte("model = \"untouched\"\n[mcp_servers.echo]\ncommand = \"node\"\nargs = [\"server.js\"]\n")
	}
	writeTestFile(t, native.path, string(data))
	f := adoptionFixture{target: native, before: data}
	if skills {
		f.canonicalSkill = filepath.Join(home, ".agentcfg", "skills", "review")
		f.nativeSkill = filepath.Join(native.skills, "review")
		for _, root := range []string{f.canonicalSkill, f.nativeSkill} {
			writeTestFile(t, filepath.Join(root, "SKILL.md"), "---\nname: review\ndescription: Review changes safely.\n---\nOriginal skill instructions.\n")
			writeTestFile(t, filepath.Join(root, "references", "rules.txt"), "Preserve all source bytes.\n")
		}
	}
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg = cfg
	f.opts = Options{
		Root: home, Home: home, Binary: filepath.Join(home, "bin", "agentcfg.exe"), Scope: "user", SkillMode: "copy", AdoptExisting: true,
		AdoptMCP: []MCPAdoption{{Harness: harness, Path: native.path, Name: "echo", CanonicalName: "echo", FileSHA256: rawDigest(data), CanonicalSHA256: ServerSHA256(cfg.Servers["echo"])}},
	}
	if skills {
		hash, err := SkillTreeSHA256(f.nativeSkill)
		if err != nil {
			t.Fatal(err)
		}
		f.opts.AdoptSkills = []SkillAdoption{{Harness: harness, Path: f.nativeSkill, CanonicalName: "review", TreeSHA256: hash}}
	}
	return f
}

func assertAdoptionBytes(t *testing.T, path string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("unexpected bytes at %s: %q, err=%v", path, actual, err)
	}
}

func assertNoAdoptionManifest(t *testing.T, home string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(home, ".agentcfg", "sync-user.json")); !os.IsNotExist(err) {
		t.Fatalf("ownership manifest should be absent: %v", err)
	}
}

func TestMCPInactiveProofMustMatchObservedNativePolicy(t *testing.T) {
	f := newAdoptionFixture(t, "cursor", false)
	f.opts.AdoptMCP[0].Inactive = true
	server := f.cfg.Servers["echo"]
	server.Harnesses = []string{"none"}
	f.cfg.Servers["echo"] = server
	if _, err := BuildPlan(f.cfg, f.opts); err == nil {
		t.Fatal("inactive proof allowed an active native entry to be retired")
	}
	assertAdoptionBytes(t, f.target.path, f.before)
	assertNoAdoptionManifest(t, f.opts.Home)
}

func TestAdoptionRequiresConsentAndSpecificProof(t *testing.T) {
	for _, mode := range []string{"default", "proof-without-consent", "consent-without-proof", "unproved-skill"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdoptionFixture(t, "cursor", mode == "unproved-skill")
			switch mode {
			case "default":
				f.opts.AdoptExisting, f.opts.AdoptMCP = false, nil
			case "proof-without-consent":
				f.opts.AdoptExisting = false
			case "consent-without-proof":
				f.opts.AdoptMCP = nil
			case "unproved-skill":
				f.opts.AdoptSkills = nil
			}
			if _, err := BuildPlan(f.cfg, f.opts); err == nil {
				t.Fatal("unapproved native collision was accepted")
			}
			assertAdoptionBytes(t, f.target.path, f.before)
			assertNoAdoptionManifest(t, f.opts.Home)
		})
	}
}

func TestAdoptNativeJSONConfigurations(t *testing.T) {
	for _, harness := range []string{"claude-code", "cursor", "opencode", "gemini", "antigravity"} {
		t.Run(harness, func(t *testing.T) {
			f := newAdoptionFixture(t, harness, false)
			plan, err := BuildPlan(f.cfg, f.opts)
			if err != nil {
				t.Fatal(err)
			}
			assertAdoptionBytes(t, f.target.path, f.before)
			assertNoAdoptionManifest(t, f.opts.Home)
			found := false
			for _, change := range plan.Changes {
				found = found || change.Path == f.target.path && strings.Contains(change.Action, "adopt")
			}
			if !found || !strings.Contains(strings.Join(plan.Warnings, "\n"), "explicitly adopt native MCP") {
				t.Fatal("preview did not disclose the adoption")
			}
			if err := plan.Apply(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(f.target.path)
			if err != nil {
				t.Fatal(err)
			}
			after, err := uniqueJSONObject(data)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := uniqueJSONObject(f.before)
			beforeServers := before[f.target.key].(map[string]any)
			afterServers := after[f.target.key].(map[string]any)
			if digest(afterServers["manual"]) != digest(beforeServers["manual"]) || digest(before["theme"]) != digest(after["theme"]) || !bytes.Contains(data, []byte("// Keep this native comment.")) {
				t.Fatal("adoption modified unrelated native content")
			}
			if digest(afterServers["echo"]) != digest(proxyEntry(f.target, f.opts.Binary, f.cfg.Root, "echo")) {
				t.Fatalf("native MCP was not converted to the expected proxy: %v", afterServers["echo"])
			}
			backups, err := filepath.Glob(filepath.Join(f.opts.Home, ".agentcfg", "backups", "*", "*-"+filepath.Base(f.target.path)))
			if err != nil || len(backups) != 1 {
				t.Fatalf("original native backup missing: %v %v", backups, err)
			}
			assertAdoptionBytes(t, backups[0], f.before)
			if len(plan.state.Configs[f.target.path]) != 1 || plan.state.Configs[f.target.path]["echo"] == "" {
				t.Fatal("manifest did not own only the converted MCP")
			}
			f.opts.AdoptExisting, f.opts.AdoptMCP = false, nil
			second, err := BuildPlan(f.cfg, f.opts)
			if err != nil || len(second.Changes) != 0 {
				t.Fatalf("ordinary sync after adoption is not idempotent: plan=%+v err=%v", second, err)
			}
		})
	}
}

func TestAdoptionRenamesOnlyApprovedNativeJSONKey(t *testing.T) {
	f := newAdoptionFixture(t, "cursor", false)
	nativeName := "echo/~legacy"
	f.before = bytes.ReplaceAll(f.before, []byte(`"echo":`), []byte(`"echo/~legacy":`))
	writeTestFile(t, f.target.path, string(f.before))
	f.opts.AdoptMCP[0].Name = nativeName
	f.opts.AdoptMCP[0].FileSHA256 = rawDigest(f.before)
	plan, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(f.target.path)
	document, _ := uniqueJSONObject(data)
	entries := document[f.target.key].(map[string]any)
	if entries[nativeName] != nil || entries["echo"] == nil || entries["manual"] == nil {
		t.Fatalf("incorrect approved-key rename: %v", entries)
	}
}

func TestAdoptionRefusesInvalidMCPProofs(t *testing.T) {
	tests := map[string]func(*adoptionFixture){
		"stale-file":            func(f *adoptionFixture) { writeTestFile(t, f.target.path, string(f.before)+"\n") },
		"stale-canonical":       func(f *adoptionFixture) { f.opts.AdoptMCP[0].CanonicalSHA256 = strings.Repeat("0", 64) },
		"missing-file-hash":     func(f *adoptionFixture) { f.opts.AdoptMCP[0].FileSHA256 = "" },
		"missing-native-name":   func(f *adoptionFixture) { f.opts.AdoptMCP[0].Name = "absent" },
		"unselected-harness":    func(f *adoptionFixture) { f.opts.AdoptMCP[0].Harness = "gemini" },
		"wrong-path":            func(f *adoptionFixture) { f.opts.AdoptMCP[0].Path = filepath.Join(f.opts.Home, "other.json") },
		"relative-path":         func(f *adoptionFixture) { f.opts.AdoptMCP[0].Path = ".cursor/mcp.json" },
		"duplicate-proof":       func(f *adoptionFixture) { f.opts.AdoptMCP = append(f.opts.AdoptMCP, f.opts.AdoptMCP[0]) },
		"project-scope":         func(f *adoptionFixture) { f.opts.Scope = "project" },
		"other-home":            func(f *adoptionFixture) { f.opts.Home = t.TempDir() },
		"missing-explicit-home": func(f *adoptionFixture) { f.opts.Home = "" },
		"stale-loaded-config":   func(f *adoptionFixture) { f.cfg.Servers["echo"] = config.Server{Type: "stdio", Command: "changed"} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			f := newAdoptionFixture(t, "cursor", false)
			mutate(&f)
			if _, err := BuildPlan(f.cfg, f.opts); err == nil {
				t.Fatal("invalid adoption proof was accepted")
			}
			assertNoAdoptionManifest(t, f.cfg.Root)
		})
	}
}

func TestAdoptionDoesNotAuthorizeAnotherCollision(t *testing.T) {
	f := newAdoptionFixture(t, "cursor", false)
	writeTestFile(t, filepath.Join(f.cfg.Root, ".agentcfg", "mcp.json"), `{"mcpServers":{"echo":{"command":"node","args":["server.js"]},"manual":{"command":"node"}}}`)
	var err error
	f.cfg, err = config.Load(f.cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildPlan(f.cfg, f.opts); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("proof authorized an unrelated colliding MCP: %v", err)
	}
	assertAdoptionBytes(t, f.target.path, f.before)
}

func TestAdoptionRefusesAmbiguousJSON(t *testing.T) {
	inputs := []string{
		`{"mcpServers":{"echo":{"command":"node"}},"mcpServers":{"echo":{"command":"node"}}}`,
		`{"mcpServers":{"echo":{"command":"node"},"\u0065cho":{"command":"node"}}}`,
		`{"mcpServers":{"echo":{"command":"node","command":"other"}}}`,
	}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			f := newAdoptionFixture(t, "cursor", false)
			writeTestFile(t, f.target.path, input)
			f.opts.AdoptMCP[0].FileSHA256 = rawDigest([]byte(input))
			if _, err := BuildPlan(f.cfg, f.opts); err == nil || !strings.Contains(err.Error(), "duplicate JSON key") {
				t.Fatalf("ambiguous JSON was adopted: %v", err)
			}
			assertAdoptionBytes(t, f.target.path, []byte(input))
		})
	}
}

func TestAdoptionSupportsNativeTOMLWithoutPlanningWrites(t *testing.T) {
	f := newAdoptionFixture(t, "codex", false)
	if _, err := BuildPlan(f.cfg, f.opts); err != nil {
		t.Fatal(err)
	}
	assertAdoptionBytes(t, f.target.path, f.before)
	assertNoAdoptionManifest(t, f.opts.Home)
}

func TestAdoptEqualSkillPreservesCanonicalSourceAndBackup(t *testing.T) {
	f := newAdoptionFixture(t, "cursor", true)
	sourceBytes, _ := os.ReadFile(filepath.Join(f.canonicalSkill, "SKILL.md"))
	if err := os.Chmod(filepath.Join(f.nativeSkill, "SKILL.md"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	assertAdoptionBytes(t, filepath.Join(f.canonicalSkill, "SKILL.md"), sourceBytes)
	assertAdoptionBytes(t, filepath.Join(f.nativeSkill, "SKILL.md"), sourceBytes)
	backups, _ := filepath.Glob(filepath.Join(f.opts.Home, ".agentcfg", "backups", "*", "*-review"))
	if len(backups) != 1 {
		t.Fatalf("native skill backup missing: %v", backups)
	}
	hash, err := SkillTreeSHA256(backups[0])
	if err != nil || hash != f.opts.AdoptSkills[0].TreeSHA256 {
		t.Fatalf("native skill backup differs from the imported tree: %v", err)
	}
	f.opts.AdoptExisting, f.opts.AdoptMCP, f.opts.AdoptSkills = false, nil, nil
	next, err := BuildPlan(f.cfg, f.opts)
	if err != nil || len(next.Changes) != 0 {
		t.Fatalf("ordinary skill sync after adoption is not idempotent: %+v %v", next, err)
	}
}

func TestAdoptionRequiresEverySkillAssetToMatch(t *testing.T) {
	for _, changed := range []string{"native-asset", "source-asset", "native-empty-dir"} {
		t.Run(changed, func(t *testing.T) {
			f := newAdoptionFixture(t, "cursor", true)
			switch changed {
			case "native-asset":
				writeTestFile(t, filepath.Join(f.nativeSkill, "references", "rules.txt"), "A native edit.\n")
			case "source-asset":
				writeTestFile(t, filepath.Join(f.canonicalSkill, "references", "rules.txt"), "A canonical edit.\n")
			default:
				if err := os.Mkdir(filepath.Join(f.nativeSkill, "extra"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := BuildPlan(f.cfg, f.opts); err == nil || !strings.Contains(err.Error(), "byte-identical") {
				t.Fatalf("nonidentical skill was adopted: %v", err)
			}
			assertAdoptionBytes(t, f.target.path, f.before)
			assertNoAdoptionManifest(t, f.opts.Home)
		})
	}
}

func TestAdoptionRechecksProofsBeforeAnyWrite(t *testing.T) {
	for _, changed := range []string{"native-json", "native-skill", "source-skill", "canonical-config"} {
		t.Run(changed, func(t *testing.T) {
			f := newAdoptionFixture(t, "cursor", true)
			plan, err := BuildPlan(f.cfg, f.opts)
			if err != nil {
				t.Fatal(err)
			}
			expectedJSON := f.before
			switch changed {
			case "native-json":
				expectedJSON = append(append([]byte{}, f.before...), '\n')
				writeTestFile(t, f.target.path, string(expectedJSON))
			case "native-skill":
				writeTestFile(t, filepath.Join(f.nativeSkill, "references", "rules.txt"), "A native edit after preview.\n")
			case "source-skill":
				writeTestFile(t, filepath.Join(f.canonicalSkill, "references", "rules.txt"), "A source edit after preview.\n")
			default:
				writeTestFile(t, filepath.Join(f.cfg.Root, ".agentcfg", "mcp.json"), `{"mcpServers":{"echo":{"command":"changed"}}}`)
			}
			if err := plan.Apply(); err == nil {
				t.Fatal("stale adoption preview was applied")
			}
			assertAdoptionBytes(t, f.target.path, expectedJSON)
			assertNoAdoptionManifest(t, f.opts.Home)
		})
	}
}

func TestAdoptionRechecksOpenCodePreferredPath(t *testing.T) {
	f := newAdoptionFixture(t, "opencode", false)
	plan, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, strings.TrimSuffix(f.target.path, "c"), "{}")
	if err := plan.Apply(); err == nil || !strings.Contains(err.Error(), "native destination") {
		t.Fatalf("stale OpenCode destination was applied: %v", err)
	}
	assertAdoptionBytes(t, f.target.path, f.before)
	assertNoAdoptionManifest(t, f.opts.Home)
}

func TestAdoptionCannotOverrideManagedDrift(t *testing.T) {
	f := newAdoptionFixture(t, "cursor", false)
	plan, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, f.target.path, string(f.before))
	if _, err := BuildPlan(f.cfg, f.opts); err == nil || !strings.Contains(err.Error(), "already managed") {
		t.Fatalf("adoption overrode managed drift: %v", err)
	}
	assertAdoptionBytes(t, f.target.path, f.before)
}

func TestAdoptionChecksIdenticalProxyBeforeTakingOwnership(t *testing.T) {
	f := newAdoptionFixture(t, "cursor", false)
	document := map[string]any{f.target.key: map[string]any{"echo": proxyEntry(f.target, f.opts.Binary, f.cfg.Root, "echo")}}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, f.target.path, string(data))
	f.opts.AdoptMCP[0].FileSHA256 = rawDigest(data)
	plan, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, change := range plan.Changes {
		if change.Path == f.target.path {
			found = change.adopted && bytes.Equal(change.before, change.after)
		}
	}
	if !found {
		t.Fatal("an identical adopted entry skipped its guarded transaction")
	}
	edited := append(append([]byte{}, data...), '\n')
	writeTestFile(t, f.target.path, string(edited))
	if err := plan.Apply(); err == nil {
		t.Fatal("manifest-only adoption ignored a concurrent native edit")
	}
	assertAdoptionBytes(t, f.target.path, edited)
	assertNoAdoptionManifest(t, f.opts.Home)
}

func TestAdoptionRejectsReplacedHomeBeforeCreatingLock(t *testing.T) {
	f := newAdoptionFixture(t, "cursor", false)
	plan, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	original := f.opts.Home + "-original"
	if err := os.Rename(f.opts.Home, original); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(f.opts.Home); err != nil {
			t.Error(err)
		}
		if err := os.Rename(original, f.opts.Home); err != nil {
			t.Error(err)
		}
	}()
	if err := os.Mkdir(f.opts.Home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err == nil || !strings.Contains(err.Error(), "adoption home changed") {
		t.Fatalf("replaced physical home was accepted: %v", err)
	}
	entries, err := os.ReadDir(f.opts.Home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected adoption wrote to the replacement home: %v %v", entries, err)
	}
}

func TestAdoptionRefusesInvalidSkillProofs(t *testing.T) {
	for _, mode := range []string{"wrong-path", "wrong-harness", "wrong-name", "missing-hash", "duplicate", "absent-native"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdoptionFixture(t, "cursor", true)
			switch mode {
			case "wrong-path":
				f.opts.AdoptSkills[0].Path = filepath.Join(f.opts.Home, "review")
			case "wrong-harness":
				f.opts.AdoptSkills[0].Harness = "gemini"
			case "wrong-name":
				f.opts.AdoptSkills[0].CanonicalName = "absent"
			case "missing-hash":
				f.opts.AdoptSkills[0].TreeSHA256 = ""
			case "duplicate":
				f.opts.AdoptSkills = append(f.opts.AdoptSkills, f.opts.AdoptSkills[0])
			case "absent-native":
				if err := os.RemoveAll(f.nativeSkill); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := BuildPlan(f.cfg, f.opts); err == nil {
				t.Fatal("invalid skill adoption proof was accepted")
			}
			assertAdoptionBytes(t, f.target.path, f.before)
			assertNoAdoptionManifest(t, f.opts.Home)
		})
	}
}

func TestAdoptionRollsBackNativeFilesAndSkills(t *testing.T) {
	f := newAdoptionFixture(t, "cursor", true)
	beforeSkill, err := treeFingerprint(f.nativeSkill)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	barrier := filepath.Join(f.opts.Home, ".agentcfg", "rollback-barrier")
	plan.Changes = append(plan.Changes,
		Change{Path: barrier, Action: "test late failure", after: []byte("regular file")},
		Change{Path: filepath.Join(barrier, "child.json"), Action: "test late failure", after: []byte("{}")},
	)
	if err := plan.Apply(); err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("expected deterministic transaction rollback, got %v", err)
	}
	assertAdoptionBytes(t, f.target.path, f.before)
	afterSkill, err := treeFingerprint(f.nativeSkill)
	if err != nil || beforeSkill != afterSkill {
		t.Fatalf("native skill was not restored exactly: %v", err)
	}
	if _, err := os.Stat(barrier); !os.IsNotExist(err) {
		t.Fatalf("rollback left its earlier test change behind: %v", err)
	}
	assertNoAdoptionManifest(t, f.opts.Home)
}

func TestAdoptedSkillRollbackPreservesExternalEdit(t *testing.T) {
	f := newAdoptionFixture(t, "cursor", true)
	plan, err := BuildPlan(f.cfg, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	var change Change
	for _, candidate := range plan.Changes {
		if candidate.Path == f.nativeSkill {
			change = candidate
		}
	}
	backup := filepath.Join(f.opts.Home, ".agentcfg", "test-backup", "review")
	undo, err := plan.applySkill(change, backup)
	if err != nil {
		t.Fatal(err)
	}
	edited := []byte("An external edit after installation.\n")
	writeTestFile(t, filepath.Join(f.nativeSkill, "references", "rules.txt"), string(edited))
	if err := undo(); err == nil || !strings.Contains(err.Error(), "externally edited") {
		t.Fatalf("rollback overwrote a later edit: %v", err)
	}
	assertAdoptionBytes(t, filepath.Join(f.nativeSkill, "references", "rules.txt"), edited)
	hash, err := SkillTreeSHA256(backup)
	if err != nil || hash != f.opts.AdoptSkills[0].TreeSHA256 {
		t.Fatalf("recoverable native backup was lost: %v", err)
	}
}
