package tui

import (
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// Huh Note renders all its content and advances on any key; Height only pads
// it. Keep its rendering and accessible output, adding a native Bubble viewport
// so long init/sync previews can be read without spilling over the next form.
type scrollableNote struct {
	*huh.Note
	viewport viewport.Model
}

func scrollNotes(fields []huh.Field) []huh.Field {
	decorated := append([]huh.Field(nil), fields...)
	for i, field := range decorated {
		if note, ok := field.(*huh.Note); ok {
			decorated[i] = &scrollableNote{Note: note, viewport: viewport.New(0, visibleMenuOptions)}
		}
	}
	return decorated
}

func (s *scrollableNote) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if input, ok := msg.(tea.KeyMsg); ok {
		s.viewport.SetContent(s.Note.View())
		switch input.String() {
		case "home", "g":
			s.viewport.GotoTop()
			return s, nil
		case "end", "G":
			s.viewport.GotoBottom()
			return s, nil
		case "up", "down", "j", "k", "pgup", "pgdown", "ctrl+u", "ctrl+d":
			var cmd tea.Cmd
			s.viewport, cmd = s.viewport.Update(msg)
			return s, cmd
		}
	}
	_, cmd := s.Note.Update(msg)
	return s, cmd
}

func (s *scrollableNote) View() string {
	view := s.Note.View()
	s.viewport.SetContent(view)
	s.viewport.Width = lipgloss.Width(view)
	if lipgloss.Height(view) <= s.viewport.Height {
		return view
	}
	return s.viewport.View()
}

func (s *scrollableNote) WithHeight(height int) huh.Field {
	s.viewport.Height = max(1, height)
	return s
}

func (s *scrollableNote) KeyBinds() []key.Binding {
	if lipgloss.Height(s.Note.View()) <= s.viewport.Height {
		return s.Note.KeyBinds()
	}
	return append([]key.Binding{
		key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "up")),
		key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "down")),
	}, s.Note.KeyBinds()...)
}
