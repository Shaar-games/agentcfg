package tui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentmux/internal/config"
	"agentmux/internal/syncer"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func userSyncFixture(t *testing.T) (Model, string, string, string, string) {
	t.Helper()
	root := t.TempDir()
	skills := filepath.Join(root, ".agentmux", "skills")
	primary := filepath.Join(skills, "review", "SKILL.md")
	initial := "---\nname: review\ndescription: Check things\nmetadata:\n  harness: codex # keep\n  author: demo\n---\n# Review\n\nThis content must be identical after installation.\n"
	none := filepath.Join(skills, "disabled", "SKILL.md")
	noneData := "---\nname: disabled\ndescription: Disabled skill\nmetadata:\n  harness: none\n---\nDo not alter this source file.\n"
	for path, data := range map[string]string{primary: initial, none: noneData} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	settings := filepath.Join(root, ".agentmux", "agentmux.json")
	if err := os.WriteFile(settings, []byte(`{"harnesses":["codex"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil || len(cfg.Warnings) != 0 || len(cfg.Skills) != 2 {
		t.Fatalf("invalid sync fixture: %v %+v", err, cfg)
	}
	svc := defaultServices()
	svc.previewSync = func(ctx context.Context, root string) (*syncer.Plan, error) {
		current, err := config.Load(root)
		if err != nil {
			return nil, err
		}
		return syncer.BuildPlan(current, syncer.Options{
			Root: current.Root, Home: current.Root, Scope: "user", Binary: filepath.Join(root, "agentmux.exe"), SkillMode: "auto",
		})
	}
	m := newModel(context.Background(), root, svc)
	model, _ := m.Update(loadedMsg{config: cfg})
	return model.(Model), root, primary, none, initial
}

func assertNoSyncWrites(t *testing.T, root string) {
	t.Helper()
	for _, path := range []string{
		filepath.Join(root, ".agents"),
		filepath.Join(root, ".codex"),
		filepath.Join(root, ".cursor"),
		filepath.Join(root, ".agentmux", "sync-user.json"),
		filepath.Join(root, ".agentmux", "sync.lock"),
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("sync preview/cancel wrote %s: %v", path, err)
		}
	}
}

func TestSyncPreviewCancelAndApplyDoesNotRewriteSkillBytes(t *testing.T) {
	m, root, source, disabled, original := userSyncFixture(t)
	probe := filepath.Join(root, "symlink-probe")
	supportsSymlinks := os.Symlink(filepath.Dir(source), probe) == nil
	if supportsSymlinks {
		if err := os.Remove(probe); err != nil {
			t.Fatal(err)
		}
	}
	beforeDisabled, err := os.ReadFile(disabled)
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := press(t, m, "s")
	if !m.syncModal.open || !m.syncModal.loading || cmd == nil {
		t.Fatal("s should open an asynchronous read-only preview")
	}
	assertNoSyncWrites(t, root)
	message, ok := cmd().(syncPreviewMsg)
	if !ok || message.err != nil || message.plan == nil {
		t.Fatalf("preparing sync preview failed: %+v", message)
	}
	model, _ := m.Update(message)
	m = model.(Model)
	if m.syncModal.loading || !strings.Contains(m.View(), "Planned changes") {
		t.Fatalf("preview not visible:\n%s", m.View())
	}
	assertNoSyncWrites(t, root)
	m, _ = press(t, m, "esc")
	if m.syncModal.open {
		t.Fatal("escape did not cancel preview")
	}
	assertNoSyncWrites(t, root)

	m, cmd = press(t, m, "s")
	model, _ = m.Update(cmd())
	m = model.(Model)
	if !strings.Contains(m.View(), "y apply changes") {
		t.Fatalf("sync preview did not require confirmation: %s", m.View())
	}
	assertNoSyncWrites(t, root)
	m, cmd = press(t, m, "y")
	if cmd == nil || !m.syncModal.applying {
		t.Fatal("y did not begin explicitly confirmed apply")
	}
	assertNoSyncWrites(t, root)
	// Applying never rewrites the canonical source skill or its frontmatter.
	model, ignored := press(t, m, "esc")
	if ignored != nil || !model.(Model).syncModal.applying {
		t.Fatal("sync should not silently cancel an active apply")
	}
	applied, ok := cmd().(syncAppliedMsg)
	if !ok || applied.err != nil {
		t.Fatalf("sync apply failed: %+v", applied)
	}
	model2, reload := m.Update(applied)
	m = model2.(Model)
	if m.syncModal.open || reload == nil || !strings.Contains(m.notice, "without modifying") {
		t.Fatalf("apply did not close modal and show result: %+v", m)
	}
	installed := filepath.Join(root, ".agents", "skills", "review", "SKILL.md")
	if supportsSymlinks {
		info, err := os.Lstat(filepath.Dir(installed))
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("auto mode should prefer symlinks when supported: %v %+v", err, info)
		}
	}
	result, err := os.ReadFile(installed)
	if err != nil || !bytes.Equal(result, []byte(original)) {
		t.Fatalf("installed SKILL.md differs from source bytes: %q (%v)", result, err)
	}
	actualSource, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(actualSource, []byte(original)) {
		t.Fatalf("sync changed source SKILL.md: %q (%v)", actualSource, err)
	}
	actualDisabled, err := os.ReadFile(disabled)
	if err != nil || !bytes.Equal(actualDisabled, beforeDisabled) {
		t.Fatalf("sync changed disabled source SKILL.md or metadata.harness none: %q (%v)", actualDisabled, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents", "skills", "disabled")); !os.IsNotExist(err) {
		t.Fatalf("disabled skill unexpectedly installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".vscode")); !os.IsNotExist(err) {
		t.Fatal("project-scoped VS Code config was created")
	}
}

func TestSyncPreviewCollisionShowsErrorWithoutWriting(t *testing.T) {
	m, root, _, _, _ := userSyncFixture(t)
	unowned := filepath.Join(root, ".agents", "skills", "review")
	if err := os.MkdirAll(unowned, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unowned, "SKILL.md"), []byte("unmanaged skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, cmd := press(t, m, "s")
	model, _ := m.Update(cmd())
	m = model.(Model)
	if m.syncModal.err == nil || !strings.Contains(m.View(), "destination exists") {
		t.Fatalf("collision should be reported in preview: %s", m.View())
	}
	m, cmd = press(t, m, "y")
	if cmd != nil || m.syncModal.applying {
		t.Fatal("y should be ignored when preview failed")
	}
	data, err := os.ReadFile(filepath.Join(unowned, "SKILL.md"))
	if err != nil || string(data) != "unmanaged skill" {
		t.Fatalf("unowned skill modified during collision: %q (%v)", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".agentmux", "sync-user.json")); !os.IsNotExist(err) {
		t.Fatalf("failed preview wrote ownership manifest: %v", err)
	}
}

func TestSyncPreviewStaleSourceRequiresFreshPlan(t *testing.T) {
	m, _, source, _, original := userSyncFixture(t)
	m, cmd := press(t, m, "s")
	model, _ := m.Update(cmd())
	m = model.(Model)
	if err := os.WriteFile(source, []byte(original+"# a concurrent edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, cmd = press(t, m, "y")
	if cmd == nil {
		t.Fatal("expected an explicit apply command after y")
	}
	model, _ = m.Update(cmd())
	m = model.(Model)
	if m.syncModal.err == nil || !strings.Contains(m.syncModal.err.Error(), "changed") || m.syncModal.plan != nil {
		t.Fatalf("stale source should stop apply and request a new preview: %+v", m.syncModal)
	}
	m, cmd = press(t, m, "y")
	if cmd != nil {
		t.Fatal("stale plan can be applied again without preview")
	}
	m, cmd = press(t, m, "r")
	if cmd == nil || !m.syncModal.loading {
		t.Fatal("r must rebuild the preview")
	}
	model, _ = m.Update(cmd())
	m = model.(Model)
	if m.syncModal.err != nil || m.syncModal.plan == nil || m.syncModal.loading {
		t.Fatalf("refresh did not rebuild preview: %+v", m.syncModal)
	}
}

func TestSyncPreviewLoadingCancelAndSmallTerminal(t *testing.T) {
	m, _, _, _, _ := userSyncFixture(t)
	model, _ := m.Update(tea.WindowSizeMsg{Width: 38, Height: 13})
	m = model.(Model)
	m, cmd := press(t, m, "s")
	first := cmd
	if cmd == nil || !strings.Contains(m.View(), "Preparing preview") {
		t.Fatalf("loading state is unavailable: %s", m.View())
	}
	for _, line := range strings.Split(m.View(), "\n") {
		if lipgloss.Width(line) > m.width {
			t.Errorf("modal overflow at 38 columns: %q", line)
		}
	}
	m, _ = press(t, m, "esc")
	if m.syncModal.open {
		t.Fatal("cancel did not close loading modal")
	}
	// A delayed completion from the canceled preview must be discarded.
	model, _ = m.Update(first())
	m = model.(Model)
	if m.syncModal.open {
		t.Fatal("stale result from canceled preview reopened modal")
	}
	model, _ = m.Update(tea.WindowSizeMsg{Width: 42, Height: 18})
	m = model.(Model)
	m, cmd = press(t, m, "s")
	model, _ = m.Update(cmd())
	m = model.(Model)
	for _, line := range strings.Split(m.View(), "\n") {
		if lipgloss.Width(line) > m.width {
			t.Errorf("preview overflow at 42 columns: %q", line)
		}
	}
}

func TestSyncPreviewWarningsAndNoChanges(t *testing.T) {
	m, root, _, _, _ := userSyncFixture(t)
	m.service.previewSync = func(context.Context, string) (*syncer.Plan, error) {
		return &syncer.Plan{Warnings: []string{"Duplicate plugin component skipped"}}, nil
	}
	m.service.applySync = func(*syncer.Plan) error { return errors.New("apply must not be called") }
	m, cmd := press(t, m, "s")
	model, _ := m.Update(cmd())
	m = model.(Model)
	if !strings.Contains(m.View(), "Warnings") || !strings.Contains(m.View(), "Nothing to install") {
		t.Fatalf("warnings-only preview is incomplete: %s", m.View())
	}
	m, cmd = press(t, m, "y")
	if cmd != nil {
		t.Fatal("no-change preview allowed apply")
	}
	assertNoSyncWrites(t, root)
}
