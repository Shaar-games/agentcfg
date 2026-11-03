//go:build darwin

package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/pathutil"
)

func TestDiscoveryCapturesMacOSTemporaryDirectoryAlias(t *testing.T) {
	physical, err := pathutil.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(physical, "/private/var/") {
		t.Skip("temporary directory is not under the macOS /var alias")
	}
	home := strings.TrimPrefix(physical, "/private")
	a, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(physical)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, b) {
		t.Fatal("temporary aliases do not identify the same directory")
	}
	native := put(t, home, ".cursor/mcp.json", `{"mcpServers":{"fixture":{"command":"node"}}}`)
	put(t, home, ".cursor/skills/review/SKILL.md", skill("review"))
	plan, err := Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Home != home || len(plan.Servers) != 1 || len(plan.Skills) != 1 || plan.Servers[0].Origins[0].Source != native {
		t.Fatalf("native /var origin changed: %+v", plan)
	}
	x, okX := plan.SourceSHA256(native)
	y, okY := plan.SourceSHA256(filepath.Join(physical, ".cursor", "mcp.json"))
	if !okX || !okY || x != y {
		t.Fatal("macOS native and physical spellings lost the captured proof")
	}
	if err := plan.Revalidate(); err != nil {
		t.Fatal(err)
	}
	if err := VerifySkill(physical, plan.Skills[0]); err != nil {
		t.Fatal(err)
	}
}
