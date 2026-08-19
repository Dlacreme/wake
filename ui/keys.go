package ui

import "github.com/charmbracelet/bubbles/key"

// Navigation and list-mode keys use raw letters (vim-style).
// ctrl is only used where the user is actively typing (grep prompt, note editor).
type keyMap struct {
	Up         key.Binding // k / ↑
	Down       key.Binding // j / ↓
	Enter      key.Binding // enter — open in editor
	Viewed     key.Binding // v — mark viewed
	Toggle     key.Binding // t — diff/full toggle
	Grep       key.Binding // / — grep changed files
	FileList   key.Binding // f — back to file list
	Peek       key.Binding // p — peek whole repo
	Refresh    key.Binding // r — refresh
	Zoom       key.Binding // z — zoom preview full-screen
	Quit       key.Binding // q / ctrl-c
	Esc        key.Binding // esc — back / cancel
	Note       key.Binding // n — add/edit note
	NotesList  key.Binding // N — view all notes
	ViewedList key.Binding // V — view viewed files
	Publish    key.Binding // P — publish PR review
	Help       key.Binding // H — help popup
	FocusNext  key.Binding // l — focus preview pane
	FocusPrev  key.Binding // h — focus list pane
	// typing-mode bindings (grep prompt, note editor)
	Submit   key.Binding // ctrl-s — save note
	GrepExec key.Binding // enter — execute grep
}

var keys = keyMap{
	Up: key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("k/↑", "up"),
	),
	Down: key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("j/↓", "down"),
	),
	Enter: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "open"),
	),
	Viewed: key.NewBinding(
		key.WithKeys("v"),
		key.WithHelp("v", "viewed"),
	),
	Toggle: key.NewBinding(
		key.WithKeys("t"),
		key.WithHelp("t", "diff/full"),
	),
	Grep: key.NewBinding(
		key.WithKeys("/"),
		key.WithHelp("/", "grep"),
	),
	FileList: key.NewBinding(
		key.WithKeys("f"),
		key.WithHelp("f", "file list"),
	),
	Peek: key.NewBinding(
		key.WithKeys("p"),
		key.WithHelp("p", "peek"),
	),
	Refresh: key.NewBinding(
		key.WithKeys("r"),
		key.WithHelp("r", "refresh"),
	),
	Zoom: key.NewBinding(
		key.WithKeys("z"),
		key.WithHelp("z", "zoom preview"),
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
		key.WithHelp("N", "notes"),
	),
	ViewedList: key.NewBinding(
		key.WithKeys("V"),
		key.WithHelp("V", "viewed list"),
	),
	Publish: key.NewBinding(
		key.WithKeys("P"),
		key.WithHelp("P", "publish"),
	),
	Help: key.NewBinding(
		key.WithKeys("H"),
		key.WithHelp("H", "help"),
	),
	FocusNext: key.NewBinding(
		key.WithKeys("l"),
		key.WithHelp("l", "focus preview"),
	),
	FocusPrev: key.NewBinding(
		key.WithKeys("h"),
		key.WithHelp("h", "focus list"),
	),
	Submit: key.NewBinding(
		key.WithKeys("ctrl+s"),
		key.WithHelp("ctrl-s", "save"),
	),
	GrepExec: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "search"),
	),
}
