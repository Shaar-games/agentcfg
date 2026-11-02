package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agentmux/internal/config"
	"agentmux/internal/syncer"

	tea "github.com/charmbracelet/bubbletea"
)

// syncDialog is read-only until y is pressed. Its plan is a snapshot prepared
// against the actual user-scope target (never project scope) and the Apply
// method rechecks each source and destination before modifying anything.
type syncDialog struct {
	open, loading, applying bool
	token                   uint64
	plan                    *syncer.Plan
	err                     error
	scroll                  int
}

// buildUserSyncPlan uses the same canonical planner as `agentmux sync`.
// Crucially, it passes an explicit user scope so it cannot install into
// project-local folders. The planner does not mutate any files.
func buildUserSyncPlan(ctx context.Context, root string) (*syncer.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	binary, err := stableExecutable()
	if err != nil {
		return nil, err
	}
	plan, err := syncer.BuildPlan(cfg, syncer.Options{
		Root: cfg.Root, Home: cfg.Root, Scope: "user", Binary: binary, SkillMode: "auto",
	})
	if err != nil {
		return nil, err
	}
	return plan, ctx.Err()
}

// In particular, `go run` produces a disposable go-build executable and must
// never be installed into a long-lived native MCP command configuration.
func stableExecutable() (string, error) {
	binary, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate installed agentmux binary: %w", err)
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return "", fmt.Errorf("resolve installed agentmux binary: %w", err)
	}
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("agentmux binary must exist as a regular installed file: %w", err)
	}
	temporary, err := filepath.Abs(os.TempDir())
	if err != nil {
		return "", err
	}
	if relative, err := filepath.Rel(temporary, binary); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		for _, segment := range strings.Split(relative, string(filepath.Separator)) {
			if strings.HasPrefix(segment, "go-build") {
				return "", fmt.Errorf("cannot install a temporary 'go run' binary into harness configurations; install agentmux first")
			}
		}
	}
	return binary, nil
}

func applyUserSyncPlan(plan *syncer.Plan) error {
	if plan == nil {
		return fmt.Errorf("sync plan is missing; build a preview first")
	}
	return plan.Apply()
}

func (m Model) openSyncModal() (tea.Model, tea.Cmd) {
	if m.loading || m.busy || m.loadError != nil || m.config == nil {
		return m, nil
	}
	m.syncToken++
	m.syncModal = syncDialog{open: true, loading: true, token: m.syncToken}
	return m, m.previewSyncCmd()
}

func (m Model) previewSyncCmd() tea.Cmd {
	token := m.syncModal.token
	return func() tea.Msg {
		plan, err := m.service.previewSync(m.ctx, m.root)
		return syncPreviewMsg{token: token, plan: plan, err: err}
	}
}

func (m Model) handleSyncModalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.syncModal.applying {
		// An already confirmed application cannot safely be interrupted by
		// dismissing the preview. Its result stays visible in the same TUI.
		return m, nil
	}
	switch msg.String() {
	case "esc":
		m.syncModal = syncDialog{}
		return m, nil
	case "up", "k":
		m.syncModal.scroll = max(0, m.syncModal.scroll-1)
	case "down", "j":
		m.syncModal.scroll = min(max(0, len(m.syncPreviewLines())-1), m.syncModal.scroll+1)
	case "home", "g":
		m.syncModal.scroll = 0
	case "end", "G":
		m.syncModal.scroll = max(0, len(m.syncPreviewLines())-1)
	case "r":
		if !m.syncModal.loading {
			m.syncToken++
			m.syncModal = syncDialog{open: true, loading: true, token: m.syncToken}
			return m, m.previewSyncCmd()
		}
	case "y":
		if m.syncModal.loading || m.syncModal.plan == nil || m.syncModal.err != nil {
			return m, nil
		}
		if len(m.syncModal.plan.Changes) == 0 {
			return m, nil
		}
		plan, token := m.syncModal.plan, m.syncModal.token
		m.syncModal.applying = true
		return m, func() tea.Msg {
			return syncAppliedMsg{token: token, count: len(plan.Changes), err: m.service.applySync(plan)}
		}
	}
	return m, nil
}
