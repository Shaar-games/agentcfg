package syncer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/config"
)

func TestPlanRejectsCanonicalSettingsChangedAfterPreview(t *testing.T) {
	home := t.TempDir()
	canonical := filepath.Join(home, ".agentcfg")
	if err := os.MkdirAll(canonical, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "mcp.json"), []byte(`{"mcpServers":{"test":{"type":"stdio","command":"node"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(canonical, "agentcfg.json")
	if err := os.WriteFile(settings, []byte(`{"harnesses":["cursor"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(cfg, Options{Root: home, Home: home, Binary: filepath.Join(home, "agentcfg"), Scope: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected preview changes")
	}
	if plan.sourceConfigDigest == "" {
		t.Fatal("canonical config snapshot was not acquired")
	}
	if err := os.WriteFile(settings, []byte(`{"harnesses":["codex"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	err = plan.Apply()
	if err == nil || !strings.Contains(err.Error(), "configuration changed") {
		t.Fatalf("stale canonical configuration applied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".cursor", "mcp.json")); !os.IsNotExist(err) {
		t.Fatal("stale plan wrote native configuration")
	}
}
