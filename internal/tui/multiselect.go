package tui

import (
	"fmt"
	"io"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// Back is an action, never a checkbox value. Keep Huh's real MultiSelect for
// toggles, filters and submission, and let Back use the form's cancellation
// path so pending selections cannot reach the saving/confirmation code.
type backMultiSelect struct {
	*huh.MultiSelect[string]
	options       []huh.Option[string]
	form          *huh.Form
	renderer      *lipgloss.Renderer
	back          bool
	backRequested bool
	optionRows    int
}

func (s interfaceSession) multiSelectWithBack(field *huh.MultiSelect[string], options []huh.Option[string]) huh.Field {
	view := &backMultiSelect{MultiSelect: field, options: options}
	if s.navigationSupportsColor() {
		view.renderer = lipgloss.NewRenderer(s.out)
	}
	return view
}

func (s *backMultiSelect) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if input, ok := msg.(tea.KeyMsg); ok && input.String() == "tab" && s.GetFiltering() {
		_, _ = s.MultiSelect.Update(tea.KeyMsg{Type: tea.KeyEsc})
		_, _ = s.MultiSelect.Update(tea.KeyMsg{Type: tea.KeyEsc})
		s.back = true
		return s, nil
	}
	if input, ok := msg.(tea.KeyMsg); ok && !s.GetFiltering() {
		if s.back {
			switch input.String() {
			case "enter":
				s.backRequested = true
				if s.form != nil {
					_, cmd := s.form.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
					return s, cmd
				}
			case "up", "k", "shift+tab":
				s.back = false
				_, cmd := s.MultiSelect.Update(tea.KeyMsg{Type: tea.KeyEnd})
				return s, cmd
			case "down", "j", "home", "g", "tab", "esc":
				s.back = false
				_, cmd := s.MultiSelect.Update(tea.KeyMsg{Type: tea.KeyHome})
				return s, cmd
			}
			return s, nil
		}
		if input.String() == "tab" || input.String() == "esc" {
			s.back = true
			return s, nil
		}
		switch input.String() {
		case "up", "down", "j", "k":
			before, ok := s.Hovered()
			_, cmd := s.MultiSelect.Update(msg)
			after, stillHovered := s.Hovered()
			if ok && stillHovered && before == after {
				s.back = true
			}
			return s, cmd
		}
	}
	_, cmd := s.MultiSelect.Update(msg)
	return s, cmd
}

func (s *backMultiSelect) View() string {
	view := s.MultiSelect.View()

	label := "Back"
	if s.renderer != nil {
		label = s.renderer.NewStyle().Foreground(navBackColor).Render(label)
	}
	return renderPinnedAction(view, s.headerHeight(), s.optionRows, label, s.back && !s.GetFiltering())
}

func (s *backMultiSelect) KeyBinds() []key.Binding {
	if s.back {
		return []key.Binding{
			key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "up")),
			key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "down")),
			key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "back")),
		}
	}
	return append(s.MultiSelect.KeyBinds(), key.NewBinding(key.WithKeys("esc", "tab"), key.WithHelp("esc/tab", "back")))
}

func (s *backMultiSelect) headerHeight() int {
	probe := *s.MultiSelect
	probe.Height(1)
	return lipgloss.Height(probe.View()) - 1
}

func (s *backMultiSelect) setOptionRows(rows int) { s.optionRows = rows }

func (s *backMultiSelect) RunAccessible(w io.Writer, r io.Reader) error {
	if _, err := fmt.Fprintln(w, "b + Enter: Back without saving."); err != nil {
		return err
	}
	input := &backChoiceReader{r: r}
	err := s.MultiSelect.RunAccessible(w, input)
	if input.back {
		s.backRequested = true
		return huh.ErrUserAborted
	}
	return err
}

// Huh's accessible MultiSelect accepts 0 to finish. Translate the explicit Back
// shortcut into that exit, then return cancellation instead of a submitted
// selection. One-byte reads preserve answers for following screens.
type backChoiceReader struct {
	r    io.Reader
	back bool
}

func (r *backChoiceReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n, err := r.r.Read(p[:1])
	if n == 1 && (p[0] == 'b' || p[0] == 'B') {
		r.back = true
		p[0] = '0'
	}
	return n, err
}

// Huh calls RunAccessible on the value returned by this fluent setter.
func (s *backMultiSelect) WithAccessible(accessible bool) huh.Field {
	s.MultiSelect.WithAccessible(accessible)
	return s
}
