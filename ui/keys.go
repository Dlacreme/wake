package ui

import "github.com/charmbracelet/bubbles/key"

type keyMap struct {
	Up       key.Binding
	Down     key.Binding
	Enter    key.Binding
	Viewed   key.Binding // v — mark file viewed
	Toggle   key.Binding // ctrl-d — diff/full
	Grep     key.Binding // ctrl-g
	FileList key.Binding // ctrl-f
	Peek     key.Binding // ctrl-p
	Refresh  key.Binding // ctrl-r
	Layout   key.Binding // ctrl-/
	Quit     key.Binding // ctrl-c / q
	Esc      key.Binding
}

var keys = keyMap{
	Up: key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("↑/k", "up"),
	),
	Down: key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("↓/j", "down"),
	),
	Enter: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "open"),
	),
	Viewed: key.NewBinding(
		key.WithKeys("v"),
		key.WithHelp("v", "mark viewed"),
	),
	Toggle: key.NewBinding(
		key.WithKeys("ctrl+d"),
		key.WithHelp("ctrl-d", "diff/full"),
	),
	Grep: key.NewBinding(
		key.WithKeys("ctrl+g"),
		key.WithHelp("ctrl-g", "grep"),
	),
	FileList: key.NewBinding(
		key.WithKeys("ctrl+f"),
		key.WithHelp("ctrl-f", "file list"),
	),
	Peek: key.NewBinding(
		key.WithKeys("ctrl+p"),
		key.WithHelp("ctrl-p", "peek"),
	),
	Refresh: key.NewBinding(
		key.WithKeys("ctrl+r"),
		key.WithHelp("ctrl-r", "refresh"),
	),
	Layout: key.NewBinding(
		key.WithKeys("ctrl+/"),
		key.WithHelp("ctrl-/", "layout"),
	),
	Quit: key.NewBinding(
		key.WithKeys("ctrl+c", "q"),
		key.WithHelp("q", "quit"),
	),
	Esc: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "back"),
	),
}
