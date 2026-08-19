package ui

import "github.com/charmbracelet/bubbles/key"

type keyMap struct {
	Up        key.Binding
	Down      key.Binding
	Enter     key.Binding
	Viewed    key.Binding // v — mark file viewed
	Toggle    key.Binding // ctrl-t — diff/full
	Grep      key.Binding // ctrl-g
	FileList  key.Binding // ctrl-f
	Peek      key.Binding // ctrl-p
	Refresh   key.Binding // ctrl-r
	Layout    key.Binding // ctrl-/
	Quit      key.Binding // ctrl-c / q
	Esc       key.Binding
	Note      key.Binding // n — add/edit note on current file
	NotesList key.Binding // N — view all pending notes
	Publish   key.Binding // ctrl-s — publish notes as PR review
	Submit    key.Binding // ctrl-s inside note editor — save note
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
		key.WithKeys("ctrl+t"),
		key.WithHelp("ctrl-t", "diff/full"),
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
	Note: key.NewBinding(
		key.WithKeys("n"),
		key.WithHelp("n", "note"),
	),
	NotesList: key.NewBinding(
		key.WithKeys("N"),
		key.WithHelp("N", "all notes"),
	),
	Publish: key.NewBinding(
		key.WithKeys("ctrl+s"),
		key.WithHelp("ctrl-s", "publish review"),
	),
	Submit: key.NewBinding(
		key.WithKeys("ctrl+s"),
		key.WithHelp("ctrl-s", "save note"),
	),
}
