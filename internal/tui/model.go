// Package tui provides the interactive AgentMux terminal interface.
// It only reads the broker state; opening the interface never starts MCPs.
package tui

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"agentmux/internal/broker"
	"agentmux/internal/config"
	"agentmux/internal/syncer"

	tea "github.com/charmbracelet/bubbletea"
)

const refreshInterval = 7 * time.Second

type section uint8

const (
	harnessSection section = iota
	mcpSection
	skillSection
	pluginSection
	diagnosticSection
	sectionCount
)

var sections = [...]string{"Harnesses", "MCP servers", "Skills", "Plugins", "Diagnostics"}

type harnessInfo struct {
	ID, Label, ConfigFile, SkillDirectory string
	UserScopeEnabled                      bool
	Note                                  string
}

// Harness registrations here are the user-scope native destinations. VS Code
// requires a profile/project scope; it is discoverable but deliberately locked.
var harnesses = [...]harnessInfo{
	{"claude-code", "Claude Code", "~/.claude.json", "~/.claude/skills", true, ""},
	{"codex", "Codex", "~/.codex/config.toml", "~/.agents/skills", true, ""},
	{"cursor", "Cursor", "~/.cursor/mcp.json", "~/.cursor/skills", true, ""},
	{"opencode", "OpenCode", "~/.config/opencode/opencode.json", "~/.config/opencode/skills", true, "An existing opencode.jsonc is used when applicable."},
	{"gemini", "Gemini CLI", "~/.gemini/settings.json", "~/.gemini/skills", true, ""},
	{"antigravity", "Antigravity", "~/.gemini/config/mcp_config.json", "~/.gemini/config/skills", true, ""},
	{"vscode", "VS Code", "Project/profile configuration", "Project/profile skills", false, "User-scope VS Code sync is unavailable; use a supported project/profile workflow."},
}

type services struct {
	load        func(string) (*config.Config, error)
	status      func(context.Context, string) (broker.Status, error)
	toggle      func(string, string, bool, bool) error
	share       func(string, string, string, string) error
	editSkill   func(string, config.Skill, string, bool) error
	listPlugins func(string, *config.Config) []pluginInfo
	previewSync func(context.Context, string) (*syncer.Plan, error)
	applySync   func(*syncer.Plan) error
}

func defaultServices() services {
	return services{
		load: config.Load, status: broker.GetStatus,
		toggle: setHarnessEnabled, share: setServerSharing,
		editSkill: setSkillHarness, listPlugins: discoverPlugins,
		previewSync: buildUserSyncPlan, applySync: applyUserSyncPlan,
	}
}

type skillDialog struct {
	open     bool
	skill    config.Skill
	options  []harnessInfo
	selected int
}

// Model is a Bubble Tea state machine. NewModel can be exercised without a TTY.
type Model struct {
	root     string
	ctx      context.Context
	service  services
	section  section
	selected [sectionCount]int
	scroll   [sectionCount]int
	width    int
	height   int

	config     *config.Config
	status     broker.Status
	plugins    []pluginInfo
	loadError  error
	statusErr  error
	busy       bool
	loading    bool
	showHelp   bool
	showDetail bool
	notice     string
	spinner    int
	skillModal skillDialog
	syncModal  syncDialog
	syncToken  uint64
}

// NewModel returns the interactive home dashboard. root must be the canonical
// configuration source root (the caller supplies the user's home directory).
func NewModel(root string) Model {
	return newModel(context.Background(), root, defaultServices())
}

func newModel(ctx context.Context, root string, service services) Model {
	return Model{
		root: root, ctx: ctx, service: service, width: 88, height: 28,
		loading: true, showDetail: true, plugins: []pluginInfo{},
		status: broker.Status{Backends: []broker.BackendStatus{}},
	}
}

// Run starts the full-screen Bubble Tea application. It uses the streams
// supplied by the CLI, keeping MCP stdio proxy operations entirely separate.
func Run(ctx context.Context, root string, input io.Reader, output io.Writer) error {
	options := []tea.ProgramOption{tea.WithContext(ctx), tea.WithAltScreen()}
	if input != nil {
		options = append(options, tea.WithInput(input))
	}
	if output != nil {
		options = append(options, tea.WithOutput(output))
	}
	_, err := tea.NewProgram(newModel(ctx, root, defaultServices()), options...).Run()
	return err
}

type loadedMsg struct {
	config  *config.Config
	status  broker.Status
	plugins []pluginInfo
	err     error
	statErr error
}

type statusMsg struct {
	status broker.Status
	err    error
}

type savedMsg struct{ err error }
type syncPreviewMsg struct {
	token uint64
	plan  *syncer.Plan
	err   error
}
type syncAppliedMsg struct {
	token uint64
	count int
	err   error
}
type refreshMsg time.Time
type spinMsg time.Time

func (m Model) loadCmd() tea.Cmd {
	return func() tea.Msg {
		cfg, err := m.service.load(m.root)
		status, statusErr := m.service.status(m.ctx, m.root)
		var plugins []pluginInfo
		if err == nil {
			plugins = m.service.listPlugins(cfg.Root, cfg)
		}
		return loadedMsg{config: cfg, status: status, plugins: plugins, err: err, statErr: statusErr}
	}
}

func (m Model) statusCmd() tea.Cmd {
	return func() tea.Msg {
		status, err := m.service.status(m.ctx, m.root)
		return statusMsg{status: status, err: err}
	}
}

func refreshCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return refreshMsg(t) })
}

func spinCmd() tea.Cmd {
	return tea.Tick(135*time.Millisecond, func(t time.Time) tea.Msg { return spinMsg(t) })
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.loadCmd(), refreshCmd(), spinCmd())
}

func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(38, msg.Width), max(13, msg.Height)
		m.clampSelection()
		return m, nil
	case loadedMsg:
		m.config, m.status, m.loadError, m.statusErr = msg.config, msg.status, msg.err, msg.statErr
		m.plugins, m.loading, m.busy = msg.plugins, false, false
		m.clampSelection()
		if m.skillModal.open {
			if msg.err != nil || msg.config == nil {
				m.skillModal = skillDialog{}
			} else {
				var found bool
				for _, skill := range msg.config.Skills {
					if skill.Name == m.skillModal.skill.Name && skill.Path == m.skillModal.skill.Path {
						m.skillModal.skill = skill
						found = true
						break
					}
				}
				if !found {
					m.skillModal = skillDialog{}
					m.notice = "Selected skill changed or disappeared; refresh before editing."
				} else {
					m.skillModal.options = m.skillOptions()
					if len(m.skillModal.options) == 0 {
						m.skillModal = skillDialog{}
					} else {
						m.skillModal.selected = min(m.skillModal.selected, len(m.skillModal.options)-1)
					}
				}
			}
		}
		return m, nil
	case statusMsg:
		m.status, m.statusErr = msg.status, msg.err
		return m, nil
	case savedMsg:
		m.busy = false
		if msg.err != nil {
			m.notice = "Change rejected: " + msg.err.Error()
			return m, nil
		}
		m.notice, m.loading = "Saved. Run 'agentmux sync --apply' to update harness files.", true
		return m, m.loadCmd()
	case syncPreviewMsg:
		if !m.syncModal.open || msg.token != m.syncModal.token {
			return m, nil
		}
		m.syncModal.loading = false
		m.syncModal.plan, m.syncModal.err = msg.plan, msg.err
		m.syncModal.scroll = 0
		return m, nil
	case syncAppliedMsg:
		if !m.syncModal.open || msg.token != m.syncModal.token {
			return m, nil
		}
		m.syncModal.applying = false
		if msg.err != nil {
			m.syncModal.plan = nil // Must preview again before a second attempt.
			m.syncModal.err = fmt.Errorf("sync failed: %w; press r to rebuild preview", msg.err)
			return m, nil
		}
		m.syncModal = syncDialog{}
		m.notice = fmt.Sprintf("Installed %d planned change(s). Skills were copied or linked without modifying their content.", msg.count)
		m.loading = true
		return m, m.loadCmd()
	case refreshMsg:
		if m.loading || m.busy {
			return m, refreshCmd()
		}
		return m, tea.Batch(refreshCmd(), m.statusCmd())
	case spinMsg:
		m.spinner = (m.spinner + 1) % 4
		return m, spinCmd()
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.syncModal.applying {
		return m.handleSyncModalKey(msg)
	}
	switch msg.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	}
	if m.syncModal.open {
		return m.handleSyncModalKey(msg)
	}
	if m.skillModal.open && !m.showHelp {
		return m.handleSkillModalKey(msg)
	}
	switch msg.String() {
	case "?":
		m.showHelp = !m.showHelp
		return m, nil
	case "esc":
		m.showHelp = false
		return m, nil
	}
	if m.showHelp {
		return m, nil
	}
	switch msg.String() {
	case "tab", "right", "l":
		m.section = (m.section + 1) % sectionCount
		m.notice = ""
	case "shift+tab", "left", "h":
		m.section = (m.section + sectionCount - 1) % sectionCount
		m.notice = ""
	case "1", "2", "3", "4", "5":
		m.section = section(msg.Runes[0] - '1')
		m.notice = ""
	case "up", "k":
		if m.selected[m.section] > 0 {
			m.selected[m.section]--
		}
	case "down", "j":
		if m.selected[m.section] < m.itemCount()-1 {
			m.selected[m.section]++
		}
	case "home", "g":
		m.selected[m.section] = 0
	case "end", "G":
		m.selected[m.section] = max(0, m.itemCount()-1)
	case "r":
		if !m.loading && !m.busy {
			m.notice, m.loading = "", true
			return m, m.loadCmd()
		}
	case "d":
		m.showDetail = !m.showDetail
	case "enter", " ":
		return m.activate()
	case "i":
		if m.section == mcpSection {
			return m.switchSharing()
		}
	case "a":
		if m.section == skillSection {
			return m.openSkillModal()
		}
	case "s":
		if m.loadError == nil && m.config != nil && !m.loading && !m.busy {
			return m.openSyncModal()
		}
	}
	m.clampSelection()
	return m, nil
}

func (m Model) activate() (tea.Model, tea.Cmd) {
	if m.loading || m.busy || m.config == nil {
		return m, nil
	}
	switch m.section {
	case harnessSection:
		entry := harnesses[m.selected[harnessSection]]
		if !entry.UserScopeEnabled {
			m.notice = entry.Note
			return m, nil
		}
		old := slices.Contains(m.config.Settings.Harnesses, entry.ID)
		m.busy, m.notice = true, "Saving harness settings..."
		return m, func() tea.Msg {
			return savedMsg{err: m.service.toggle(m.root, entry.ID, !old, old)}
		}
	case mcpSection:
		m.showDetail = !m.showDetail
	case skillSection:
		return m.openSkillModal()
	case pluginSection:
		m.showDetail = !m.showDetail
	}
	return m, nil
}

func (m Model) skillOptions() []harnessInfo {
	if m.config == nil {
		return nil
	}
	var options []harnessInfo
	for _, harness := range harnesses {
		if harness.UserScopeEnabled && slices.Contains(m.config.Settings.Harnesses, harness.ID) {
			options = append(options, harness)
		}
	}
	return options
}

func (m Model) openSkillModal() (tea.Model, tea.Cmd) {
	if m.config == nil || m.loading || m.busy || len(m.config.Skills) == 0 {
		return m, nil
	}
	options := m.skillOptions()
	if len(options) == 0 {
		m.notice = "Enable a supported harness before changing a skill's availability."
		return m, nil
	}
	m.skillModal = skillDialog{open: true, skill: m.config.Skills[m.selected[skillSection]], options: options}
	m.notice = ""
	return m, nil
}

func (m Model) handleSkillModalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		if !m.busy {
			m.skillModal = skillDialog{}
			m.notice = ""
		}
	case "up", "k", "left", "h":
		if !m.busy {
			m.skillModal.selected = max(0, m.skillModal.selected-1)
		}
	case "down", "j", "right", "l":
		if !m.busy {
			m.skillModal.selected = min(len(m.skillModal.options)-1, m.skillModal.selected+1)
		}
	case " ", "enter":
		if m.loading || m.busy || m.config == nil || len(m.skillModal.options) == 0 {
			return m, nil
		}
		option := m.skillModal.options[m.skillModal.selected]
		skill := m.skillModal.skill
		newValue := !config.Allows(skill.Harnesses, option.ID)
		m.busy, m.notice = true, "Saving skill availability..."
		return m, func() tea.Msg {
			return savedMsg{err: m.service.editSkill(m.root, skill, option.ID, newValue)}
		}
	}
	return m, nil
}

func (m Model) switchSharing() (tea.Model, tea.Cmd) {
	if m.loading || m.busy || m.config == nil || len(m.config.Servers) == 0 {
		return m, nil
	}
	name := m.serverNames()[m.selected[mcpSection]]
	old := sharing(m.config.Servers[name])
	next := "isolated"
	if old == "isolated" {
		next = "shared"
	}
	m.busy, m.notice = true, fmt.Sprintf("Saving %s sharing mode...", name)
	return m, func() tea.Msg {
		return savedMsg{err: m.service.share(m.root, name, next, old)}
	}
}

func sharing(server config.Server) string {
	if server.Sharing == "isolated" {
		return "isolated"
	}
	return "shared"
}

func (m Model) serverNames() []string {
	if m.config == nil {
		return nil
	}
	names := make([]string, 0, len(m.config.Servers))
	for name := range m.config.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m Model) itemCount() int {
	switch m.section {
	case harnessSection:
		return len(harnesses)
	case mcpSection:
		return len(m.serverNames())
	case skillSection:
		if m.config != nil {
			return len(m.config.Skills)
		}
	case pluginSection:
		return len(m.plugins)
	case diagnosticSection:
		if m.config != nil {
			return len(m.config.Warnings)
		}
	}
	return 0
}

func (m *Model) clampSelection() {
	for i := section(0); i < sectionCount; i++ {
		active := m.section
		m.section = i
		m.selected[i] = min(m.selected[i], max(0, m.itemCount()-1))
		m.section = active
	}
}

func (m Model) activeBackend(name string) (broker.BackendStatus, bool) {
	var result broker.BackendStatus
	var found bool
	for _, b := range m.status.Backends {
		if b.Name == name {
			if !found {
				result = b
				found = true
			} else {
				result.Clients += b.Clients
				result.Pending += b.Pending
			}
		}
	}
	return result, found
}

func (m Model) backendCount(name string) int {
	count := 0
	for _, b := range m.status.Backends {
		if b.Name == name {
			count++
		}
	}
	return count
}

func enabledForHarness(enabled []string, name string) bool {
	return slices.Contains(enabled, name)
}

func (m Model) availableSkills(harness string) int {
	if m.config == nil || !enabledForHarness(m.config.Settings.Harnesses, harness) {
		return 0
	}
	count := 0
	for _, skill := range m.config.Skills {
		if config.Allows(skill.Harnesses, harness) {
			count++
		}
	}
	return count
}

func (m Model) availableServers(harness string) int {
	if m.config == nil || !enabledForHarness(m.config.Settings.Harnesses, harness) {
		return 0
	}
	count := 0
	for _, server := range m.config.Servers {
		if config.Allows(server.Harnesses, harness) {
			count++
		}
	}
	return count
}

func (m Model) enabledHarnesses(filter []string) string {
	if m.config == nil {
		return "—"
	}
	var names []string
	for _, info := range harnesses {
		if info.UserScopeEnabled && slices.Contains(m.config.Settings.Harnesses, info.ID) && config.Allows(filter, info.ID) {
			names = append(names, info.Label)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
