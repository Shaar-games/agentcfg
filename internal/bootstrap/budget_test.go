package bootstrap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoveryAggregateServerBudget(t *testing.T) {
	home := t.TempDir()
	entries := map[string]any{}
	for i := 0; i < 1025; i++ {
		entries[fmt.Sprintf("fixture-%04d", i)] = map[string]string{"command": "node"}
	}
	data, _ := json.Marshal(map[string]any{"mcpServers": entries})
	put(t, home, ".cursor/mcp.json", string(data))
	plan, err := Discover(home)
	if err != nil || len(plan.Servers) != 1024 || len(plan.Warnings) != 1 {
		t.Fatalf("servers=%d warnings=%+v err=%v", len(plan.Servers), plan.Warnings, err)
	}
}

func TestDiscoveryAggregateSkillBudget(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{"first", "second", "third"} {
		put(t, home, ".cursor/skills/"+name+"/SKILL.md", skill(name))
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	plan := &Plan{Home: home, budget: newScanBudget()}
	plan.budget.skills = 2
	discoverSkills(root, plan, "cursor", filepath.Join(home, ".cursor", "skills"))
	if len(plan.Skills) != 2 || len(plan.Warnings) != 1 {
		t.Fatalf("%+v", plan)
	}
}

func TestRejectedSkillReadConsumesBudget(t *testing.T) {
	home := t.TempDir()
	put(t, home, "invalid/SKILL.md", string([]byte{255, 255, 255}))
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	budget := newScanBudget()
	budget.bytes = 3
	_, _, _, _, err = inspectSkill(root, "invalid", "invalid", budget)
	if err == nil || budget.bytes != 0 {
		t.Fatalf("err=%v remaining=%d", err, budget.bytes)
	}
}

func TestRevalidateRejectsRetargetedNativeSkillAlias(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	source := filepath.Dir(put(t, home, "packs/review/SKILL.md", skill("review")))
	put(t, outside, "SKILL.md", skill("review"))
	native := filepath.Join(home, ".cursor", "skills", "review")
	if err := os.MkdirAll(filepath.Dir(native), 0700); err != nil {
		t.Fatal(err)
	}
	dirLink(t, native, source)
	plan, err := Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Skills) != 1 {
		t.Fatalf("%+v", plan)
	}
	if err = os.Remove(native); err != nil {
		t.Fatal(err)
	}
	dirLink(t, native, outside)
	if err = plan.Revalidate(); err == nil {
		t.Fatal("retargeted native alias accepted")
	}
}

func TestAbsoluteCommandWithSpacesIsAReferenceOnly(t *testing.T) {
	home := t.TempDir()
	command := put(t, home, "bin with spaces/server", "not executable; discovery must never run this")
	data, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"fixture": map[string]any{"command": command, "args": []string{"exact argument"}, "cwd": filepath.Dir(command)}}})
	put(t, home, ".cursor/mcp.json", string(data))
	plan, err := Discover(home)
	if err != nil || len(plan.Servers) != 1 || plan.Servers[0].Server.Command != command {
		t.Fatalf("%+v %v", plan, err)
	}
}
