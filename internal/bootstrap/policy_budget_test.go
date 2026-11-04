package bootstrap

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestDisableOnlyPoliciesRespectAggregateDiscoveryBudget(t *testing.T) {
	home := t.TempDir()
	var names []string
	for i := 0; i < newScanBudget().servers+8; i++ {
		names = append(names, fmt.Sprintf("server-%d", i))
	}
	data, err := json.Marshal(map[string]any{"disabledMcpServers": names})
	if err != nil {
		t.Fatal(err)
	}
	put(t, home, ".cursor/mcp.json", string(data))
	plan, err := Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.inactiveOrigins) > newScanBudget().servers {
		t.Fatal("disable-only records bypassed aggregate bounds")
	}
	blocked := false
	for _, warning := range plan.Warnings {
		if warning.BlocksInit([]string{"cursor"}) {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("truncated policy inventory could silently enable an omitted policy")
	}
}
