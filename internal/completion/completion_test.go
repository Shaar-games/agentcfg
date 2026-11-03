package completion

import (
	"bytes"
	"strings"
	"testing"
)

func TestGeneratesPopularShells(t *testing.T) {
	for _, shell := range Shells {
		t.Run(shell, func(t *testing.T) {
			var output bytes.Buffer
			if err := Generate(shell, &output); err != nil {
				t.Fatal(err)
			}
			script := output.String()
			for _, word := range []string{"agentcfg", "completion", "init", "harnesses", "skill", "mcp", "--plain"} {
				if !strings.Contains(script, word) {
					t.Errorf("shell script missing %q", word)
				}
			}
		})
	}
}

func TestUnknownShellDoesNotWrite(t *testing.T) {
	var b bytes.Buffer
	if Generate("unknown", &b) == nil || b.Len() != 0 {
		t.Fatal("invalid shell should fail without writing output")
	}
}
