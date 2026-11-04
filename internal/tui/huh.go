// Package tui provides the user-level interactive Huh configuration interface.
// It loads the canonical home sources; neither opening a menu nor reading
// diagnostics starts MCP processes or rewrites native harness configuration.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"agentcfg/internal/config"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
)

type harnessInfo struct {
	ID, Label, ConfigFile, SkillDirectory string
	UserScopeEnabled                      bool
	Note                                  string
}

var harnesses = [...]harnessInfo{
	{"claude-code", "Claude Code", "~/.claude.json", "~/.claude/skills", true, ""},
	{"codex", "Codex", "~/.codex/config.toml", "~/.agents/skills", true, ""},
	{"cursor", "Cursor", "~/.cursor/mcp.json", "~/.cursor/skills", true, ""},
	{"opencode", "OpenCode", "~/.config/opencode/opencode.json", "~/.config/opencode/skills", true, "Existing opencode.jsonc is used when applicable."},
	{"gemini", "Gemini CLI", "~/.gemini/settings.json", "~/.gemini/skills", true, ""},
	{"antigravity", "Antigravity", "~/.gemini/config/mcp_config.json", "~/.gemini/config/skills", true, ""},
	{"vscode", "VS Code (adapter unavailable)", "", "", false, "No supported VS Code native configuration adapter is installed."},
}

type interfaceSession struct {
	ctx  context.Context
	root string
	in   io.Reader
	out  io.Writer
}

func (s interfaceSession) form(fields ...huh.Field) error {
	return RunForm(s.ctx, s.in, s.out, fields...)
}

// RunForm shares native Huh controls and a ten-row option viewport between
// configuration, inventory and the initialization wizard. Terminals too small
// for the bounded form use Huh's complete accessible numbered list.
func RunForm(ctx context.Context, input io.Reader, output io.Writer, fields ...huh.Field) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if input == nil {
		input = strings.NewReader("") // Never fall back to a real process stdin.
	}
	if output == nil {
		output = io.Discard // A nil writer must not use the process stdout.
	}
	fields = scrollNotes(fields)
	reader, readState := formInput(input)
	writer, writeState := formOutput(output)
	var resizedTooSmall bool
	native := isNativeTerminal(input, output)
	explicitAccessible := os.Getenv("AGENTCFG_ACCESSIBLE") == "1"
	var overflow *bool
	if native && !explicitAccessible {
		overflow = &resizedTooSmall
	}
	form, group := newNaturalFormWithOverflow(reader, writer, overflow, fields...)
	// Huh switches to a numbered accessible prompt when TERM=dumb. Windows
	// consoles can still render the native checkbox UI in that environment;
	// only override the fallback when both streams are real terminals.
	// Redirected/non-terminal streams must use the deterministic numbered Huh
	// mode regardless of TERM, not a Bubble Tea program requiring a live TTY.
	accessible := explicitAccessible || !native
	if accessible {
		form.WithAccessible(true)
	} else if runtime.GOOS == "windows" {
		if native {
			form.WithAccessible(false)
		}
	}
	if native && !accessible {
		out := output.(*os.File)
		width, terminalHeight, sizeErr := term.GetSize(out.Fd())
		if sizeErr == nil && terminalHeight > 0 && resizeNaturalForm(form, group, fields, width, terminalHeight) > terminalHeight {
			// Huh native Select cannot show more lines than the physical TTY and
			// Bubble Tea's renderer would otherwise truncate the first options.
			// Its official accessible mode prints every numbered option without
			// creating a second/foreign UI or a hidden viewport.
			form.WithAccessible(true)
			accessible = true
		}
	}
	if accessible || runtime.GOOS != "windows" && strings.EqualFold(os.Getenv("TERM"), "dumb") {
		readState, writeState = checkedAccessibleStreams(form, input, output, readState, writeState)
	}
	err := form.RunWithContext(ctx)
	if resizedTooSmall && (err == nil || errors.Is(err, huh.ErrUserAborted)) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, writeErr := fmt.Fprintln(writer, "Terminal is smaller than this menu; showing the complete numbered list."); writeErr != nil {
			return writeErr
		}
		form.WithAccessible(true)
		readState, writeState = checkedAccessibleStreams(form, input, output, readState, writeState)
		err = form.RunWithContext(ctx)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if readState != nil && readState.err != nil {
		return readState.err
	}
	if writeState != nil && writeState.err != nil {
		return writeState.err
	}
	// Huh's accessible form runner discards individual field errors. Honor an
	// explicit Back separately so it cannot turn cancellation into a save.
	for _, field := range fields {
		if multi, ok := field.(*backMultiSelect); ok && multi.backRequested {
			return huh.ErrUserAborted
		}
	}

	return err
}

func checkedAccessibleStreams(form *huh.Form, input io.Reader, output io.Writer, readState *checkedFormInput, writeState *checkedFormOutput) (*checkedFormInput, *checkedFormOutput) {
	// Native TTYs stay unwrapped for Bubble Tea's cursor/key handling, but
	// Huh's accessible Scanner needs bounded reads and observable EOF/errors.
	if readState == nil && input != nil {
		readState = &checkedFormInput{r: input}
		form.WithInput(readState)
	}
	if writeState == nil && output != nil {
		writeState = &checkedFormOutput{w: output}
	}
	if writeState != nil {
		// Huh's accessible RunAccessible prints Theme() styled titles even on a
		// real color TTY. Keep this mode strictly textual without changing the
		// global renderer or NO_COLOR for the user's other CLI sessions.
		form.WithOutput(&plainAccessibleWriter{w: writeState})
	}
	return readState, writeState
}

// plainAccessibleWriter strips ANSI control strings incrementally. A simple
// ansi.Strip per Write is insufficient because a terminal writer may receive
// ESC[...m split across separate writes. Huh accessible prompts must not leak
// SGR or cursor-control sequences even when the native theme is colored.
type plainAccessibleWriter struct {
	w     io.Writer
	state uint8
}

const (
	plainText uint8 = iota
	plainEscape
	plainCSI
	plainOSC
	plainString
	plainOSCEscape
	plainStringEscape
)

func (w *plainAccessibleWriter) Write(p []byte) (int, error) {
	clean := make([]byte, 0, len(p))
	for _, b := range p {
		switch w.state {
		case plainText:
			switch b {
			case 0x1b:
				w.state = plainEscape
			default:
				clean = append(clean, b)
			}
		case plainEscape:
			switch b {
			case '[':
				w.state = plainCSI
			case ']':
				w.state = plainOSC
			case 'P', '_', '^', 'X':
				w.state = plainString
			default:
				w.state = plainText // Drop standalone ESC command too.
			}
		case plainCSI:
			if b >= 0x40 && b <= 0x7e {
				w.state = plainText
			}
		case plainOSC:
			if b == 0x07 {
				w.state = plainText
			} else if b == 0x1b {
				w.state = plainOSCEscape
			}
		case plainString:
			if b == 0x1b {
				w.state = plainStringEscape
			}
		case plainOSCEscape:
			if b == '\\' {
				w.state = plainText
			} else if b != 0x1b {
				w.state = plainOSC
			}
		case plainStringEscape:
			if b == '\\' {
				w.state = plainText
			} else if b != 0x1b {
				w.state = plainString
			}
		}
	}
	if len(clean) != 0 {
		n, err := w.w.Write(clean)
		if err != nil {
			return 0, err
		}
		if n != len(clean) {
			return 0, io.ErrShortWrite
		}
	}
	return len(p), nil
}

func isNativeTerminal(input io.Reader, output io.Writer) bool {
	in, inOK := input.(*os.File)
	out, outOK := output.(*os.File)
	return inOK && outOK && term.IsTerminal(in.Fd()) && term.IsTerminal(out.Fd())
}

// Huh's accessible PromptString builds a new bufio.Scanner for each field.
// Without bounded reads a scanner buffers subsequent input (e.g. the next
// screen's answers), leaving the following form at EOF. It also silently
// interprets EOF as the default choice. Record EOF so no change can be saved
// when the user never answered, and preserve native TTY streams unchanged.
type checkedFormInput struct {
	r   io.Reader
	err error
}

func (r *checkedFormInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n, err := r.r.Read(p[:1])
	if err != nil {
		r.err = err
	}
	return n, err
}

func formInput(input io.Reader) (io.Reader, *checkedFormInput) {
	if input == nil {
		return nil, nil
	}
	if file, ok := input.(*os.File); ok && term.IsTerminal(file.Fd()) {
		return input, nil
	}
	r := &checkedFormInput{r: input}
	return r, r
}

type checkedFormOutput struct {
	w   io.Writer
	err error
}

func (w *checkedFormOutput) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.w.Write(p)
	if err != nil {
		w.err = err
	} else if n != len(p) {
		w.err = io.ErrShortWrite
	}
	return n, w.err
}

func formOutput(output io.Writer) (io.Writer, *checkedFormOutput) {
	if output == nil {
		return nil, nil
	}
	if file, ok := output.(*os.File); ok && term.IsTerminal(file.Fd()) {
		return output, nil
	}
	w := &checkedFormOutput{w: output}
	return w, w
}

func newNaturalForm(input io.Reader, output io.Writer, fields ...huh.Field) (*huh.Form, *huh.Group) {
	return newNaturalFormWithOverflow(input, output, nil, fields...)
}

func newNaturalFormWithOverflow(input io.Reader, output io.Writer, overflow *bool, fields ...huh.Field) (*huh.Form, *huh.Group) {
	group := huh.NewGroup(fields...)
	form := huh.NewForm(group).WithTheme(Theme())
	// Checked values are state, not a request to open halfway down a list.
	for _, field := range fields {
		switch multi := field.(type) {
		case *huh.MultiSelect[string]:
			_, _ = multi.Update(tea.KeyMsg{Type: tea.KeyHome})
		case *backMultiSelect:
			multi.form = form
			_, _ = multi.MultiSelect.Update(tea.KeyMsg{Type: tea.KeyHome})
		}
	}

	// This Bubble Tea hook modifies Huh's own dimensions, not its UI. A
	// terminal resize recalculates the option budget after text wrapping.
	options := []tea.ProgramOption{tea.WithReportFocus(), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
		switch msg := msg.(type) {
		case tea.WindowSizeMsg:
			return updateNaturalWindowSize(form, group, fields, msg, overflow)
		case tea.InterruptMsg:
			if form.State == huh.StateAborted {
				// Huh already marked this as cancelled and its final View is empty.
				// Interrupt kills Tea's renderer before that empty frame is flushed,
				// leaving the next inline form below the previous footer. Quit lets
				// Tea clear only its own surface; Huh still returns ErrUserAborted.
				return tea.QuitMsg{}
			}
		}
		return msg
	})}
	form.WithProgramOptions(options...)
	form.WithInput(input).WithOutput(output)
	resizeNaturalForm(form, group, fields, 0)
	return form, group
}

func updateNaturalWindowSize(form *huh.Form, group *huh.Group, fields []huh.Field, size tea.WindowSizeMsg, overflow *bool) tea.Msg {
	if form.State != huh.StateNormal {
		return size
	}
	natural := resizeNaturalForm(form, group, fields, size.Width, size.Height)
	if overflow != nil && size.Height > 0 && natural > size.Height {
		*overflow = true
		// Finish the inline surface through Huh's own cancellation path before
		// switching to its complete numbered list. RunForm recognizes this
		// resize-only handoff; a genuine user cancellation remains an error.
		_, _ = form.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		return tea.QuitMsg{}
	}
	return size
}

func resizeNaturalForm(form *huh.Form, group *huh.Group, fields []huh.Field, width int, terminalHeight ...int) int {
	if width > 0 {
		form.WithWidth(width)
	}
	for _, field := range fields {
		available := 0
		if len(terminalHeight) > 0 {
			available = terminalHeight[0]
		}
		rows := visibleMenuOptions
		if available > 0 {
			chrome := menuHeaderHeight(field) + 2
			if text := group.Header(); text != "" {
				chrome += lipgloss.Height(text)
			}
			if text := group.Footer(); text != "" {
				chrome += lipgloss.Height(text) + 1
			}
			rows = max(1, min(visibleMenuOptions, available-chrome))
		}
		bodyRows := rows
		if _, ok := field.(*backMultiSelect); ok {
			bodyRows = max(1, rows-1)
		}
		sizeMenuField(field, bodyRows)
		if nav, ok := field.(bottomActionLayout); ok {
			sizeNavigationRows(nav, group, available)
		}
	}

	height := lipgloss.Height(group.Content())
	if header := group.Header(); header != "" {
		height += lipgloss.Height(header)
	}
	if footer := group.Footer(); footer != "" {
		height += lipgloss.Height(footer) + 1
	}
	form.WithHeight(max(1, height))
	return max(height, lipgloss.Height(form.View()))
}

func (s interfaceSession) selectAction(title, description string, choices ...huh.Option[string]) (string, error) {
	// Navigation is single-choice: arrows move and Enter activates immediately.
	// Checkboxes are reserved for actual multi-harness availability settings.
	if len(choices) == 0 {
		return "", fmt.Errorf("%s: no action is available", title)
	}
	selected := choices[0].Value
	field := huh.NewSelect[string]().
		Title(title).
		Description(description).
		Options(choices...).
		Value(&selected)
	err := s.form(s.navigationSelect(field, choices))
	return selected, err
}

func (s interfaceSession) note(title, description string) error {
	return s.form(huh.NewNote().Title(title).Description(safeHuhText(description)))
}

// Huh Notes render Markdown. Backslashes in a native Windows path would be
// consumed as escapes. Use forward slashes for presentation, and drop terminal
// control characters from untrusted plugin descriptions or MCP error strings.
func safeHuhText(value string) string {
	value = strings.ReplaceAll(value, "\\", "/")
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
}

// confirmChange is the only approval prompt for a changed canonical setting.
// No callers write user settings or SKILL.md before receiving a Yes! result.
func (s interfaceSession) confirmChange() (bool, error) {
	var confirm bool
	err := s.form(huh.NewConfirm().
		Title("Are you sure?").
		Affirmative("Yes!").
		Negative("No.").
		Value(&confirm))
	return confirm, err
}

func sameChoiceSet(selected, current []string) bool {
	if len(selected) != len(current) {
		return false
	}
	for _, id := range selected {
		if !slices.Contains(current, id) {
			return false
		}
	}
	return true
}

func sameHarnessAvailability(selected, configured, filter []string) bool {
	for _, id := range configured {
		if slices.Contains(selected, id) != config.Allows(filter, id) {
			return false
		}
	}
	return true
}

func isCancelled(err error) bool {
	return errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled)
}

// Run opens the simple native Huh checkbox list directly. Do not place a
// single-choice menu in front of the multiselect: users should see the
// available harnesses and their current checked states immediately.
func Run(ctx context.Context, root string, input io.Reader, output io.Writer) error {
	return (interfaceSession{ctx: ctx, root: root, in: input, out: output}).configureHarnesses()
}

// RunMCP and RunSkills enter the corresponding configuration screens directly.
// They share all validation, Huh forms, and safe canonical writes with RunMenu.
func RunMCP(ctx context.Context, root string, input io.Reader, output io.Writer) error {
	err := (interfaceSession{ctx: ctx, root: root, in: input, out: output}).configureMCP()
	if isCancelled(err) {
		return nil
	}
	return err
}

func RunSkills(ctx context.Context, root string, input io.Reader, output io.Writer) error {
	err := (interfaceSession{ctx: ctx, root: root, in: input, out: output}).configureSkills()
	if isCancelled(err) {
		return nil
	}
	return err
}

// RunMenu is the complete user-level configuration entrypoint for "agentcfg config".
// Multi-choice settings still use native Huh checkboxes inside their sections.
func RunMenu(ctx context.Context, root string, input io.Reader, output io.Writer) error {
	s := interfaceSession{ctx: ctx, root: root, in: input, out: output}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		cfg, err := config.Load(root)
		if err != nil {
			if noteErr := s.note("Invalid canonical settings", err.Error()+"\nRepair ~/.agentcfg/agentcfg.json before continuing."); noteErr != nil && !isCancelled(noteErr) {
				return noteErr
			}
			return err
		}
		choice, err := s.selectAction("agentcfg  ·  Configuration", "",
			overviewNavigationOptions(cfg, len(discoverPlugins(root, cfg)))...)
		if isCancelled(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if choice == "" {
			return nil
		}
		switch choice {
		case "harnesses":
			err = s.configureHarnesses()
		case "mcp":
			err = s.configureMCP()
		case "skills":
			err = s.configureSkills()
		case "plugins":
			err = s.showPlugins()
		case "refresh":
			continue
		case "exit":
			return nil
		}
		if isCancelled(err) {
			continue
		}
		if err != nil {
			if reportErr := s.note("Operation failed", err.Error()+"\nNo automatic retry or silent overwrite."); reportErr != nil && !isCancelled(reportErr) {
				return reportErr
			}
		}
	}
}

func (s interfaceSession) configureHarnesses() error {
	cfg, err := config.Load(s.root)
	if err != nil {
		return err
	}
	choices := make([]huh.Option[string], 0, len(harnesses))
	for _, h := range harnesses {
		if !h.UserScopeEnabled {
			continue
		}
		choices = append(choices, huh.NewOption(h.Label, h.ID).
			Selected(slices.Contains(cfg.Settings.Harnesses, h.ID)))
	}
	var selected []string
	err = s.form(s.multiSelectWithBack(huh.NewMultiSelect[string]().
		Title("Harness availability").
		Description("Choose the harnesses to enable. Changes are reviewed before saving.").
		Options(choices...).
		Value(&selected), choices))
	if isCancelled(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if sameChoiceSet(selected, cfg.Settings.Harnesses) {
		return nil
	}
	confirmed, err := s.confirmChange()
	if isCancelled(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !confirmed {
		return nil
	}
	if err := setHarnessSelection(s.root, selected, cfg.Settings.Harnesses); err != nil {
		if errors.Is(err, errNoChange) {
			return nil
		}
		return err
	}
	return nil
}

func (s interfaceSession) configureMCP() error {
	for {
		cfg, err := config.Load(s.root)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(cfg.Servers))
		for name := range cfg.Servers {
			names = append(names, name)
		}
		sort.Strings(names)
		rows := make([]navigationStatus, 0, len(names))
		for _, name := range names {
			rows = append(rows, navigationStatus{title: safeHuhText(name), columns: []string{cfg.Servers[name].Type, sharing(cfg.Servers[name])}, id: navigationItemValue(name)})
		}
		choices := statusNavigationOptions(rows)
		choices = append(choices, huh.NewOption("Back", "back"))
		name, err := s.selectAction("MCP servers", "Shared reuses the upstream process; isolated keeps session state separate.", choices...)
		if isCancelled(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if name == "back" || name == "" {
			return nil
		}
		name, valid := navigationItemName(name)
		if !valid {
			return fmt.Errorf("unexpected MCP selection")
		}
		server := cfg.Servers[name]
		choice, err := s.selectAction(name, fmt.Sprintf("Transport: %s  ·  Sharing: %s", server.Type, sharing(server)),
			huh.NewOption("Harness availability", "availability"),
			huh.NewOption("Change sharing mode", "sharing"),
			huh.NewOption("Show transport and routing details", "details"),
			huh.NewOption("Back", "back"))
		if isCancelled(err) {
			continue
		}
		if err != nil {
			return err
		}
		switch choice {
		case "availability":
			if err := s.configureMCPHarnesses(server, cfg.Settings.Harnesses); err != nil {
				return err
			}
		case "sharing":
			if err := s.configureMCPSharing(name, server); err != nil {
				return err
			}
		case "details":
			var allowed []string
			for _, h := range harnesses {
				if h.UserScopeEnabled && slices.Contains(cfg.Settings.Harnesses, h.ID) && config.Allows(server.Harnesses, h.ID) {
					allowed = append(allowed, h.Label)
				}
			}
			endpoint := server.Command
			if server.Type != "stdio" {
				endpoint = server.URL
			}
			if len(endpoint) > 200 {
				endpoint = endpoint[:200] + "…"
			}
			if err := s.note("MCP details", fmt.Sprintf("Name: %s\nTransport: %s\nSharing: %s\nEndpoint/command: %s\nEnabled harnesses: %s\n\nThe broker starts only when a client connects.", name, server.Type, sharing(server), endpoint, strings.Join(allowed, ", "))); err != nil {
				return err
			}
		}
	}
}

func (s interfaceSession) configureMCPHarnesses(server config.Server, configured []string) error {
	if len(configured) == 0 {
		return s.note("No harnesses configured", "Enable a harness in agentcfg config before assigning MCP availability.")
	}
	choices := make([]huh.Option[string], 0, len(configured))
	for _, id := range configured {
		for _, h := range harnesses {
			if h.ID == id && h.UserScopeEnabled {
				choices = append(choices, huh.NewOption(h.Label, id).
					Selected(config.Allows(server.Harnesses, id)))
				break
			}
		}
	}
	var selected []string
	err := s.form(huh.NewMultiSelect[string]().
		Title("MCP availability: " + safeHuhText(server.Name)).
		Description("Unchecked MCPs are unavailable for that harness. Changes are reviewed before saving.").
		Options(choices...).
		Value(&selected))
	if isCancelled(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if sameHarnessAvailability(selected, configured, server.Harnesses) {
		return nil
	}
	confirmed, err := s.confirmChange()
	if isCancelled(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !confirmed {
		return nil
	}
	err = setServerHarnessSelection(s.root, server, selected, configured)
	if errors.Is(err, errNoChange) {
		return nil
	}
	return err
}

func (s interfaceSession) configureMCPSharing(name string, server config.Server) error {
	current := sharing(server)
	selected := current
	choices := []huh.Option[string]{
		huh.NewOption("Shared", "shared").Selected(current == "shared"),
		huh.NewOption("Isolated", "isolated").Selected(current == "isolated"),
	}
	field := huh.NewSelect[string]().
		Title("MCP sharing: " + safeHuhText(name)).
		Description("Shared reuses the upstream process; isolated keeps session state separate.").
		Options(choices...).
		Value(&selected)
	err := s.form(s.navigationSelect(field, choices))
	if isCancelled(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if selected == current {
		return nil
	}
	confirmed, err := s.confirmChange()
	if isCancelled(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !confirmed {
		return nil
	}
	return setServerSharing(s.root, name, selected, current)
}

func sharing(server config.Server) string {
	if server.Sharing == "isolated" {
		return "isolated"
	}
	return "shared"
}

func enabledForHarness(names []string, name string) bool { return slices.Contains(names, name) }

func (s interfaceSession) configureSkills() error {
	for {
		cfg, err := config.Load(s.root)
		if err != nil {
			return err
		}
		rows := make([]navigationStatus, 0, len(cfg.Skills))
		for _, skill := range cfg.Skills {
			count := 0
			for _, h := range harnesses {
				if h.UserScopeEnabled && slices.Contains(cfg.Settings.Harnesses, h.ID) && config.Allows(skill.Harnesses, h.ID) {
					count++
				}
			}
			rows = append(rows, navigationStatus{title: safeHuhText(skill.Name), detail: strconv.Itoa(count), id: navigationItemValue(skill.Name)})
		}
		choices := statusNavigationOptions(rows)
		choices = append(choices, huh.NewOption("Back", "back"))
		name, err := s.selectAction("Agent Skills", "Counts: enabled harnesses. Enter opens availability; source edits only.", choices...)
		if isCancelled(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if name == "back" || name == "" {
			return nil
		}
		name, valid := navigationItemName(name)
		if !valid {
			return fmt.Errorf("unexpected skill selection")
		}
		if err := s.configureSkill(name); err != nil {
			return err
		}
	}
}

func (s interfaceSession) configureSkill(name string) error {
	cfg, err := config.Load(s.root)
	if err != nil {
		return err
	}
	var selected *config.Skill
	for i := range cfg.Skills {
		if cfg.Skills[i].Name == name {
			selected = &cfg.Skills[i]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("skill %q no longer exists; refresh", name)
	}
	choices := make([]huh.Option[string], 0, len(cfg.Settings.Harnesses))
	var enabled []string
	for _, h := range harnesses {
		if !h.UserScopeEnabled || !slices.Contains(cfg.Settings.Harnesses, h.ID) {
			continue
		}
		active := config.Allows(selected.Harnesses, h.ID)
		choices = append(choices, huh.NewOption(h.Label, h.ID).Selected(active))
	}
	if len(choices) == 0 {
		return s.note("No harnesses configured", "Enable a harness in agentcfg config before assigning skill availability.")
	}
	err = s.form(huh.NewMultiSelect[string]().
		Title("Skill availability: " + safeHuhText(name)).
		Description("Space toggles, Enter reviews changes, Ctrl+C cancels. No selection means metadata.harness: none.").
		Options(choices...).
		Value(&enabled))
	if isCancelled(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if sameHarnessAvailability(enabled, cfg.Settings.Harnesses, selected.Harnesses) {
		return nil
	}
	confirmed, err := s.confirmChange()
	if isCancelled(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !confirmed {
		return nil
	}
	if err := setSkillHarnessForConfigured(s.root, *selected, enabled, cfg.Settings.Harnesses); err != nil {
		if errors.Is(err, errNoChange) {
			return nil
		}
		return err
	}
	return nil
}

func (s interfaceSession) showPlugins() error {
	cfg, err := config.Load(s.root)
	if err != nil {
		return err
	}
	items := discoverPlugins(s.root, cfg)
	rows := make([]navigationStatus, 0, len(items))
	for _, p := range items {
		detail := ""
		if !p.Valid {
			detail = "warning"
		}
		rows = append(rows, navigationStatus{title: safeHuhText(p.Name), columns: []string{fmt.Sprintf("%d / %d", p.MCP, p.Skills), detail}, id: p.Path})
	}
	choices := statusNavigationOptions(rows)
	choices = append(choices, huh.NewOption("Back", "back"))
	for {
		choice, err := s.selectAction("Portable Agent Plugins", "Counts: MCP / skills. Enter shows details; package files stay untouched.", choices...)
		if isCancelled(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if choice == "back" || choice == "" {
			return nil
		}
		for _, p := range items {
			if p.Path == choice {
				info := fmt.Sprintf("Path: %s\nDescription: %s\nMCP servers: %d\nSkills: %d\nStatus: %t\n%s", p.Path, p.Description, p.MCP, p.Skills, p.Valid, p.Problem)
				if err := s.note("Plugin "+p.Name, info); err != nil {
					return err
				}
			}
		}
	}
}
