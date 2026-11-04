package main

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agentcfg/internal/tui"

	"github.com/charmbracelet/huh"
)

func TestInitNativeMultiSelectKeepsPreselectedHarnesses(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var output bytes.Buffer
	selected, err := terminalHarnessSelection(context.Background(), strings.NewReader("0\n"), &output, []string{"codex", "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(selected, []string{"codex", "cursor"}) {
		t.Fatalf("default Huh choices not preserved: %v; output: %s", selected, output.String())
	}
}

func TestTerminalConfirmUsesConsistentLabelsAndDefaultsToNo(t *testing.T) {
	var confirmed bool
	field := terminalConfirmation("Synchronize native harnesses?", "Existing files are rechecked.", &confirmed)
	huh.NewForm(huh.NewGroup(field)).WithTheme(tui.Theme())
	view := field.View()
	if strings.Contains(view, "Are you sure?") || strings.Count(view, "Synchronize native harnesses?") != 1 {
		t.Fatalf("confirmation repeats the question: %q", view)
	}
	for _, expected := range []string{"Synchronize native harnesses?", "Yes!", "No.", "Existing files are rechecked."} {
		if !strings.Contains(view, expected) {
			t.Errorf("confirmation is missing %q: %q", expected, view)
		}
	}
	if confirmed {
		t.Fatal("confirmation must default to No.")
	}
	t.Setenv("TERM", "dumb")
	t.Setenv("AGENTCFG_ACCESSIBLE", "1")
	for _, tc := range []struct {
		input string
		want  bool
	}{{"y\n", true}, {"n\n", false}, {"\n", false}} {
		var output bytes.Buffer
		got, err := terminalConfirm(context.Background(), strings.NewReader(tc.input), &output, "Save?", "Reviewed changes only.")
		if err != nil || got != tc.want {
			t.Errorf("input %q: got %t, %v; want %t", tc.input, got, err, tc.want)
		}
		if strings.Count(output.String(), "Reviewed changes only.") != 1 || strings.Count(output.String(), "Save?") != 1 || strings.Contains(output.String(), "\x1b[") {
			t.Errorf("accessible confirmation is inconsistent: %q", output.String())
		}
	}
}

func TestStopConfirmationUsesOnlyTheRequestedMessage(t *testing.T) {
	var output bytes.Buffer
	home := t.TempDir()
	if err := inspectOrServe(context.Background(), "stop", home, strings.NewReader("n\n"), &output, &bytes.Buffer{}, true, false, false); err != nil {
		t.Fatal(err)
	}
	message := "Managed MCP processes will be closed. This does not alter user configuration."
	if strings.Count(output.String(), message) != 1 || strings.Contains(output.String(), "Are you sure?") || strings.Contains(output.String(), "Stop the agentcfg broker?") {
		t.Fatalf("verbose stop confirmation: %s", output.String())
	}
}

func TestInitReviewShowsOnlySelectionAndCounts(t *testing.T) {
	summary := initReview([]string{"codex", "cursor", "opencode"}, 10, 31, 15)
	for _, value := range []string{"Codex, Cursor, OpenCode", "MCPs      10", "Skills    31", "Skipped   15", "init --plain"} {
		if !strings.Contains(summary, value) {
			t.Errorf("summary is missing %q: %s", value, summary)
		}
	}
	if len(strings.Split(summary, "\n")) > 5 {
		t.Fatalf("init review is too long: %s", summary)
	}
	for _, redundant := range []string{"Detected", "Enabled", "Importable", "Canonical", "unsupported options", "After creation"} {
		if strings.Contains(summary, redundant) {
			t.Fatalf("redundant init explanation: %s", summary)
		}
	}
}

func TestCLIFormsCannotBypassSharedNaturalRunner(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := selector.X.(*ast.Ident); ok && id.Name == "huh" && selector.Sel.Name == "NewForm" {
				t.Errorf("%s bypasses tui.RunForm and can reintroduce clipped/scrolling menus", path)
			}
			if selector.Sel.Name == "Height" || selector.Sel.Name == "WithHeight" {
				t.Errorf("%s imposes a CLI field height instead of shared natural layout", path)
			}
			return true
		})
	}
}
