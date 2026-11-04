package tui

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agentcfg/internal/config"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

func compactMenuSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			result[relative] = "directory"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[relative] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

type beforeMenuAnswer struct {
	io.Reader
	before func()
}

func (r *beforeMenuAnswer) Read(p []byte) (int, error) {
	if r.before != nil {
		before := r.before
		r.before = nil
		before()
	}
	return r.Reader.Read(p)
}

func TestCompactOverviewsRefreshCurrentCountsWithoutWriting(t *testing.T) {
	for name, run := range map[string]func(context.Context, string, io.Reader, io.Writer) error{
		"configuration": RunMenu,
		"inventory":     RunList,
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TERM", "dumb")
			t.Setenv("LOCALAPPDATA", t.TempDir())
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			root := settingsFixture(t)
			putSettings(t, root, `{"harnesses":["claude-code","codex","cursor"]}`)
			var expected map[string]string
			// Change the fixture only after the first screen has been displayed.
			// Refresh must load these new counts, not retain the first snapshot.
			input := &beforeMenuAnswer{Reader: strings.NewReader("5\n6\n"), before: func() {
				putSettings(t, root, `{"harnesses":["cursor"]}`)
				data := []byte(`{"mcpServers":{"alpha":{"command":"inert-fixture"},"beta":{"command":"inert-fixture"}}}`)
				if err := os.WriteFile(filepath.Join(root, ".agentcfg", "mcp.json"), data, 0o600); err != nil {
					t.Fatal(err)
				}
				expected = compactMenuSnapshot(t, root)
			}}
			var output bytes.Buffer
			if err := run(context.Background(), root, input, &output); err != nil {
				t.Fatal(err)
			}
			for _, row := range []string{
				"1. Harnesses      ·  3", "1. Harnesses      ·  1",
				"2. MCP servers    ·  1", "2. MCP servers    ·  2",
			} {
				if !strings.Contains(output.String(), row) {
					t.Errorf("missing current counter row %q: %s", row, output.String())
				}
			}
			for _, row := range []string{"3. Skills         ·  0", "4. Agent Plugins  ·  0", "5. Refresh", "6. Exit"} {
				if strings.Count(output.String(), row) != 2 {
					t.Errorf("each overview must contain exactly one %q: %s", row, output.String())
				}
			}
			if expected == nil || !reflect.DeepEqual(expected, compactMenuSnapshot(t, root)) {
				t.Fatal("Refresh or Exit wrote configuration, native files, locks or backups")
			}
		})
	}
}

func TestCompactOverviewRefreshAndExitUseImmediateNativeSelect(t *testing.T) {
	options := overviewNavigationOptions(&config.Config{}, 0)
	for _, action := range []struct {
		index int
		id    string
	}{{4, "refresh"}, {5, "exit"}} {
		t.Run(action.id, func(t *testing.T) {
			var selected string
			field := huh.NewSelect[string]().Title("Configuration").Options(options...).Value(&selected)
			form, group := newNaturalForm(strings.NewReader(""), io.Discard, field)
			_ = form.Init()
			resizeNaturalForm(form, group, []huh.Field{field}, 80)
			_, _ = form.Update(tea.WindowSizeMsg{Width: 80, Height: 16})
			for _, option := range options {
				if !strings.Contains(form.View(), option.Key) {
					t.Fatalf("compact menu hides %q", option.Key)
				}
			}
			if lipgloss.Height(form.View()) > 16 {
				t.Fatal("compact overview should fit naturally in a 16-row terminal")
			}
			for range action.index {
				_, _ = form.Update(tea.KeyMsg{Type: tea.KeyDown})
			}
			_, cmd := form.Update(tea.KeyMsg{Type: tea.KeyEnter})
			if selected != action.id || cmd == nil {
				t.Fatalf("Enter did not activate %s: selected=%q", action.id, selected)
			}
		})
	}
}

func TestSkillAndPluginListsUseCompactCountsAndKeepWarnings(t *testing.T) {
	t.Setenv("TERM", "dumb")
	root := settingsFixture(t)
	putSettings(t, root, `{"harnesses":["codex","cursor"]}`)
	files := map[string]string{
		"skills/review/SKILL.md":                "---\nname: review\ndescription: Fixture\nmetadata:\n  harness: codex\n---\nInert content.\n",
		"plugins/broken/plugin.json":            `{"name":"broken"}`,
		"plugins/fixture/plugin.json":           `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"fixture"}`,
		"plugins/fixture/mcp.json":              `{"$schema":"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json","mcpServers":{"first":{"type":"streamable-http","url":"https://example.invalid/first"},"second":{"type":"streamable-http","url":"https://example.invalid/second"}}}`,
		"plugins/fixture/skills/guide/SKILL.md": "---\nname: guide\ndescription: Fixture guide\n---\nInert content.\n",
	}
	for relative, contents := range files {
		path := filepath.Join(root, ".agentcfg", filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := compactMenuSnapshot(t, root)
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, inventory := range []bool{false, true} {
		var output bytes.Buffer
		session := interfaceSession{ctx: context.Background(), root: root, in: strings.NewReader("3\n"), out: &output}
		if inventory {
			err = session.listSkills(cfg)
		} else {
			err = session.configureSkills()
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "review") || !strings.Contains(output.String(), "·  1") || strings.Contains(output.String(), "harness(es)") {
			t.Fatalf("skill list has inconsistent counters: %s", output.String())
		}
	}
	var output bytes.Buffer
	session := interfaceSession{ctx: context.Background(), root: root, in: strings.NewReader("3\n"), out: &output}
	if err := session.showPlugins(); err != nil {
		t.Fatal(err)
	}
	for _, row := range []string{"broken   ·  0 / 0  ·  warning", "fixture  ·  2 / 1"} {
		if !strings.Contains(output.String(), row) {
			t.Errorf("plugin list lost compact counters or warning %q: %s", row, output.String())
		}
	}
	if !reflect.DeepEqual(before, compactMenuSnapshot(t, root)) {
		t.Fatal("browsing compact skill or plugin lists changed files")
	}
}
