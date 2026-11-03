package bootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"agentcfg/internal/pathutil"
)

// Reproduce a system directory alias (/var -> /private/var on macOS) without
// modifying a system path or inspecting a user's home.
func aliasHome(t *testing.T) (home, physical, link string) {
	t.Helper()
	base := t.TempDir()
	parent := filepath.Join(base, "physical")
	realHome := filepath.Join(parent, "user")
	if err := os.MkdirAll(realHome, 0700); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(base, "alias")
	dirLink(t, link, parent)
	physical, err := pathutil.Resolve(realHome)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(link, "user"), physical, link
}

func TestDiscoveryAliasPreservesOriginsAndCapturedHashSpellings(t *testing.T) {
	home, physical, _ := aliasHome(t)
	native := put(t, home, ".cursor/mcp.json", `{"mcpServers":{"fixture":{"command":"node"}}}`)
	put(t, home, "packs/review/SKILL.md", skill("review"))
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0700); err != nil {
		t.Fatal(err)
	}
	dirLink(t, filepath.Join(home, ".cursor", "skills"), filepath.Join(physical, "packs"))
	plan, err := Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Home != home || len(plan.Servers) != 1 || len(plan.Skills) != 1 {
		t.Fatalf("%+v", plan)
	}
	if got := plan.Servers[0].Origins[0].Source; got != native {
		t.Fatalf("MCP native origin lost: %s", got)
	}
	wantSkillOrigin := filepath.Join(home, ".cursor", "skills", "review")
	if got := plan.Skills[0].Origins[0].Source; got != wantSkillOrigin {
		t.Errorf("skill native origin lost: %s; want %s", got, wantSkillOrigin)
	}
	if got := plan.Skills[0].Source; got != filepath.Join(home, "packs", "review") {
		t.Errorf("resolved skill target has inconsistent home spelling: %s", got)
	}
	raw, err := os.ReadFile(native)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	want := hex.EncodeToString(hash[:])
	physicalSource := filepath.Join(physical, ".cursor", "mcp.json")
	for _, source := range []string{native, physicalSource} {
		if got, ok := plan.SourceSHA256(source); !ok || got != want {
			t.Errorf("captured source spelling has no matching proof: %s, got=%s ok=%v", source, got, ok)
		}
	}
	if err := plan.Revalidate(); err != nil {
		t.Fatal(err)
	}

	// A new alias must not gain a proof just because it resolves to the same
	// object. Known keys remain historical snapshots even after file removal.
	other := filepath.Join(t.TempDir(), "later-alias")
	dirLink(t, other, physical)
	if _, ok := plan.SourceSHA256(filepath.Join(other, ".cursor", "mcp.json")); ok {
		t.Fatal("a new alias received an uncaptured proof")
	}
	traversal := filepath.Join(home, ".cursor") + string(filepath.Separator) + ".." + string(filepath.Separator) + ".cursor" + string(filepath.Separator) + "mcp.json"
	if _, ok := plan.SourceSHA256(traversal); ok {
		t.Fatal("lexical parent traversal received a proof")
	}
	if err := os.Remove(native); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{native, physicalSource} {
		if got, ok := plan.SourceSHA256(source); !ok || got != want {
			t.Errorf("captured proof disappeared after file removal: %s", source)
		}
	}
	if err := plan.Revalidate(); err == nil {
		t.Fatal("removed source passed revalidation")
	}
}

func TestContainedAndAnchoredPathsAcrossRootAliases(t *testing.T) {
	home, physical, _ := aliasHome(t)
	native := put(t, home, ".cursor/mcp.json", "{}")
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, source := range []string{native, filepath.Join(physical, ".cursor", "mcp.json")} {
		if got, err := contained(home, source); err != nil || got != native {
			t.Errorf("contained alias: %s %v", got, err)
		}
		if got, err := anchoredPath(root, source); err != nil || got != filepath.Join(".cursor", "mcp.json") {
			t.Errorf("anchored alias: %s %v", got, err)
		}
	}
	outside := put(t, t.TempDir(), "outside.json", "{}")
	for _, source := range []string{outside, filepath.Join(home, "escape", "outside.json")} {
		if source != outside {
			dirLink(t, filepath.Join(home, "escape"), filepath.Dir(outside))
		}
		if _, err := contained(home, source); err == nil {
			t.Error("contained accepted an outside target")
		}
		if _, err := anchoredPath(root, source); err == nil {
			t.Error("anchoredPath accepted an outside target")
		}
	}
}

func TestRootAliasRetargetingCannotChangeTheOpenedBoundary(t *testing.T) {
	home, _, link := aliasHome(t)
	native := put(t, home, ".cursor/mcp.json", `{"mcpServers":{"fixture":{"command":"node"}}}`)
	plan, err := Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	replacement := t.TempDir()
	// Identical bytes deliberately defeat content-only revalidation.
	put(t, replacement, "user/.cursor/mcp.json", `{"mcpServers":{"fixture":{"command":"node"}}}`)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	dirLink(t, link, replacement)
	if _, err := anchoredPath(root, native); err == nil {
		t.Error("retargeted root name was treated as the opened root")
	}
	if err := plan.Revalidate(); err == nil {
		t.Error("a different home with identical files passed revalidation")
	}
}

func TestReplacingHomeAtTheSamePathInvalidatesTheAnchor(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	native := put(t, home, ".cursor/mcp.json", `{"mcpServers":{"fixture":{"command":"node"}}}`)
	plan, err := Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	// Windows refuses directory replacement while this extra handle is open.
	// Revalidate must still reject replacement between Discover and its call;
	// Unix also checks the old live anchor at the same filesystem spelling.
	if runtime.GOOS == "windows" {
		if err := root.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(home, home+"-previous"); err != nil {
		t.Fatal(err)
	}
	put(t, home, ".cursor/mcp.json", `{"mcpServers":{"fixture":{"command":"node"}}}`)
	if runtime.GOOS != "windows" {
		if _, err := anchoredPath(root, native); err == nil {
			t.Error("same path with a different directory identity was accepted")
		}
	}
	if err := plan.Revalidate(); err == nil {
		t.Error("same path with identical content bypassed home identity")
	}
}

func TestConflictingAliasCapturesCannotOverwriteAProof(t *testing.T) {
	home := t.TempDir()
	targetFile := put(t, home, "shared/mcp.json", `{"mcpServers":{"fixture":{"command":"first"}}}`)
	for _, name := range []string{"first-alias", "second-alias"} {
		dirLink(t, filepath.Join(home, name), filepath.Dir(targetFile))
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	plan := &Plan{Home: home, budget: newScanBudget()}
	configTarget := target{name: "cursor", key: "mcpServers"}
	first := filepath.Join(home, "first-alias", "mcp.json")
	second := filepath.Join(home, "second-alias", "mcp.json")
	discoverServers(root, plan, configTarget, first)
	hash, ok := plan.SourceSHA256(first)
	if !ok || len(plan.Servers) != 1 {
		t.Fatalf("missing initial capture: %+v", plan)
	}
	put(t, home, "shared/mcp.json", `{"mcpServers":{"fixture":{"command":"second"}}}`)
	discoverServers(root, plan, configTarget, second)
	physical, err := pathutil.Resolve(targetFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plan.SourceSHA256(physical); ok {
		t.Error("ambiguous physical key retained a proof")
	}
	if _, ok := plan.SourceSHA256(second); ok {
		t.Error("conflicting capture granted a native proof")
	}
	if got, ok := plan.SourceSHA256(first); !ok || got != hash {
		t.Error("an earlier native proof was overwritten")
	}
	put(t, home, "shared/mcp.json", `{"mcpServers":{"fixture":{"command":"first"}}}`)
	discoverServers(root, plan, configTarget, second)
	if _, ok := plan.SourceSHA256(physical); ok {
		t.Error("a later capture revived an ambiguous key")
	}
	if len(plan.Servers) != 1 || len(plan.Warnings) != 2 {
		t.Fatalf("conflicting source not rejected: %+v", plan)
	}
}
