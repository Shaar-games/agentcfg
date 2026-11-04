package tui

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/config"
)

func formSession(root, sequence string, output *bytes.Buffer) interfaceSession {
	return interfaceSession{
		ctx: context.Background(), root: root,
		in: terminalByteReader{strings.NewReader(sequence)}, out: output,
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func noBackupCreated(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, ".agentcfg", "backups")); !os.IsNotExist(err) {
		t.Fatalf("an aborted/no-op action must not create backups: %v", err)
	}
}

func assertConfirmBehavior(t *testing.T, output string, expect bool) {
	t.Helper()
	seen := strings.Contains(output, "Save changes?")
	if seen != expect {
		t.Fatalf("confirmation shown=%t want=%t: %q", seen, expect, output)
	}
}

func TestHarnessMultiSelectConfirmationYesNoAndNoop(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        string
		confirm     bool
	}{
		{"yes", "3\n0\ny\n", `"harnesses": [`, true},
		{"no", "3\n0\nn\n", `{"harnesses":["codex"]}`, true},
		{"unchanged", "0\n", `{"harnesses":["codex"]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERM", "dumb")
			root := settingsFixture(t)
			putSettings(t, root, `{"harnesses":["codex"]}`)
			var output bytes.Buffer
			if err := formSession(root, tc.input, &output).configureHarnesses(); err != nil {
				t.Fatal(err)
			}
			assertConfirmBehavior(t, output.String(), tc.confirm)
			data := mustRead(t, settingsFile(root))
			if tc.name == "yes" {
				cfg, err := config.Load(root)
				if err != nil || !enabledForHarness(cfg.Settings.Harnesses, "cursor") {
					t.Fatalf("yes did not save Cursor: %s (%v)", data, err)
				}
			} else {
				if string(data) != tc.want {
					t.Fatalf("declining/no-op rewrote canonical settings: %s", data)
				}
				noBackupCreated(t, root)
			}
		})
	}
}

func TestMCPAvailabilityConfirmationYesNoAndNoop(t *testing.T) {
	for _, tc := range []struct {
		name, input  string
		confirm      bool
		codexAllowed bool
	}{
		{"yes", "1\n0\ny\n", true, false},
		{"no", "1\n0\nn\n", true, true},
		{"unchanged", "0\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERM", "dumb")
			root := settingsFixture(t)
			putSettings(t, root, `{"harnesses":["codex","cursor"]}`)
			before := mustRead(t, settingsFile(root))
			cfg, err := config.Load(root)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err := formSession(root, tc.input, &output).configureMCPHarnesses(cfg.Servers["alpha"], cfg.Settings.Harnesses); err != nil {
				t.Fatal(err)
			}
			assertConfirmBehavior(t, output.String(), tc.confirm)
			if tc.name != "yes" {
				if !bytes.Equal(before, mustRead(t, settingsFile(root))) {
					t.Fatal("declining/no-op rewrote MCP settings")
				}
				noBackupCreated(t, root)
			}
			reloaded, err := config.Load(root)
			if err != nil || config.Allows(reloaded.Servers["alpha"].Harnesses, "codex") != tc.codexAllowed {
				t.Fatalf("MCP availability after confirmation: %+v, %v", reloaded.Servers["alpha"], err)
			}
		})
	}
}

func TestMCPSharingConfirmationYesNoAndNoop(t *testing.T) {
	for _, tc := range []struct {
		name, input, expected string
		confirm               bool
	}{
		{"yes", "2\ny\n", "isolated", true},
		{"no", "2\nn\n", "shared", true},
		{"unchanged", "1\n", "shared", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERM", "dumb")
			root := settingsFixture(t)
			putSettings(t, root, `{"harnesses":["codex"]}`)
			before := mustRead(t, settingsFile(root))
			cfg, err := config.Load(root)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err := formSession(root, tc.input, &output).configureMCPSharing("alpha", cfg.Servers["alpha"]); err != nil {
				t.Fatal(err)
			}
			assertConfirmBehavior(t, output.String(), tc.confirm)
			if tc.name != "yes" {
				if !bytes.Equal(before, mustRead(t, settingsFile(root))) {
					t.Fatal("declining/no-op rewrote MCP sharing")
				}
				noBackupCreated(t, root)
			}
			reloaded, err := config.Load(root)
			if err != nil || sharing(reloaded.Servers["alpha"]) != tc.expected {
				t.Fatalf("sharing after confirmation: %+v, %v", reloaded.Servers["alpha"], err)
			}
		})
	}
}

func TestSkillMetadataConfirmationYesNoAndNoop(t *testing.T) {
	const initial = "---\nname: review\ndescription: Review code\nmetadata:\n  harness: codex, antigravity\n---\n# Keep skill body unchanged\n"
	for _, tc := range []struct {
		name, input           string
		confirm, codexAllowed bool
	}{
		{"yes", "1\n0\ny\n", true, false},
		{"no", "1\n0\nn\n", true, true},
		{"unchanged", "0\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERM", "dumb")
			root, path, _ := skillSourceFixture(t, initial)
			before := mustRead(t, path)
			var output bytes.Buffer
			if err := formSession(root, tc.input, &output).configureSkill("review"); err != nil {
				t.Fatal(err)
			}
			assertConfirmBehavior(t, output.String(), tc.confirm)
			if tc.name != "yes" {
				if !bytes.Equal(before, mustRead(t, path)) {
					t.Fatal("declining/no-op rewrote SKILL.md")
				}
				noBackupCreated(t, root)
			}
			skill := loadSkillFixture(t, root, "review")
			if config.Allows(skill.Harnesses, "codex") != tc.codexAllowed {
				t.Fatalf("unexpected skill availability after confirmation: %v", skill.Harnesses)
			}
			if !strings.Contains(string(mustRead(t, path)), "# Keep skill body unchanged") {
				t.Fatal("unrelated skill content changed")
			}
		})
	}
}

// Regression test: no navigation Select may impose its own fixed viewport
// height. MultiSelect is intentionally independent because it selects a set.
func TestNavigationSelectsUseOfficialDefaultHeight(t *testing.T) {
	for _, name := range []string{"huh.go", "list.go"} {
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		selectCalls := 0
		ast.Inspect(file, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "NewSelect" {
				if id, ok := selector.X.(*ast.Ident); ok && id.Name == "huh" {
					selectCalls++
				}
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if selector.Sel.Name == "Height" && startsWithSelect(selector.X) {
				t.Errorf("%s sets a fixed height on huh.NewSelect", name)
			}
			return true
		})
		if name == "huh.go" && selectCalls < 2 {
			t.Fatalf("expected Select for navigation and MCP sharing; got %d", selectCalls)
		}
	}
}

func startsWithSelect(expr ast.Expr) bool {
	switch value := expr.(type) {
	case *ast.CallExpr:
		return startsWithSelect(value.Fun)
	case *ast.IndexExpr:
		return startsWithSelect(value.X)
	case *ast.SelectorExpr:
		if value.Sel.Name == "NewSelect" {
			id, ok := value.X.(*ast.Ident)
			return ok && id.Name == "huh"
		}
		return startsWithSelect(value.X)
	}
	return false
}
