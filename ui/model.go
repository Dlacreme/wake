package ui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/Dlacreme/wake/config"
	"github.com/Dlacreme/wake/git"
	"github.com/Dlacreme/wake/gh"
	"github.com/sahilm/fuzzy"
)

// ── modes ─────────────────────────────────────────────────────────────────────

type Mode int

const (
	ModeList       Mode = iota
	ModeGrepInput       // typing a grep query
	ModeGrep            // grep results
	ModePeek            // whole-repo fuzzy file list
	ModePeekGrep        // grep results for whole repo
	ModeNote            // note editor overlay
	ModeNotesList       // all pending notes
	ModeViewedList      // viewed files (V key)
	ModeHelp            // help popup
)

// focusPane: which pane has keyboard focus
type focusPane int

const (
	focusList    focusPane = iota
	focusPreview focusPane = iota
)

// ── messages ──────────────────────────────────────────────────────────────────

type itemsLoadedMsg struct{ items []git.Item }
type previewReadyMsg struct {
	content string
	lines   []string // split lines for click hit-testing
}
type editorDoneMsg struct{}
type errMsg struct{ err error }
type prLoadedMsg struct {
	pr       gh.PR
	comments []gh.ReviewComment
	fullDiff string
	files    []string
}
type publishDoneMsg struct{ err error }

// ── model ─────────────────────────────────────────────────────────────────────

type Model struct {
	mode  Mode
	focus focusPane

	// list
	items  []git.Item
	cursor int
	query  textinput.Model

	// viewed: path → diffHash; session-only
	viewed map[string]string

	// preview
	preview             string
	previewLines        []string // raw lines, for click line mapping
	previewScrollOffset int      // first visible line
	previewClickedRow   int      // visual row last clicked (-1 = none)
	previewClickedLine  int      // file line parsed from clicked row (0 = unknown)
	layout              previewLayout
	zoomed              bool // z key: full-screen preview (hides list)
	full                bool

	// peek
	peekItems       []git.Item
	peekCursor      int
	peekQuery       textinput.Model
	savedListCursor int
	savedListItems  []git.Item
	savedMode       Mode

	// dimensions
	width  int
	height int

	// config
	cfg   config.Config
	root  string
	since string

	// PR
	pr         *gh.PR
	prComments []gh.ReviewComment
	prFullDiff string
	prLoading  bool

	// notes
	notes    map[string]gh.Note
	noteTA   textarea.Model
	notePath string
	noteLine int // 0 = file-level

	statusMsg string
}

// ── constructor ───────────────────────────────────────────────────────────────

func New(root, since string, cfg config.Config, pr *gh.PR) Model {
	qi := textinput.New()
	qi.Placeholder = ""
	qi.Prompt = ""

	pi := textinput.New()
	pi.Placeholder = ""
	pi.Prompt = ""

	ta := textarea.New()
	ta.Placeholder = "Write your note… (ctrl-s to save, esc to cancel)"
	ta.ShowLineNumbers = false
	ta.SetWidth(80)
	ta.SetHeight(8)

	return Model{
		mode:              ModeList,
		focus:             focusList,
		viewed:            make(map[string]string),
		notes:             make(map[string]gh.Note),
		query:             qi,
		peekQuery:         pi,
		noteTA:            ta,
		cfg:               cfg,
		root:              root,
		since:             since,
		full:              cfg.Preview == "full",
		layout:            layoutFromConfig(cfg.Layout),
		pr:                pr,
		prLoading:         pr != nil,
		previewClickedRow: -1,
	}
}

// ── Init ──────────────────────────────────────────────────────────────────────

func (m Model) Init() tea.Cmd {
	if m.pr != nil {
		return loadPR(m.pr)
	}
	return loadItems(m.root, m.since, m.cfg.Exclude, m.cfg.Sort)
}

// ── Update ────────────────────────────────────────────────────────────────────

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.noteTA.SetWidth(m.width - 4)
		return m, m.refreshPreviewCmd()

	case itemsLoadedMsg:
		switch m.mode {
		case ModeList, ModeGrep, ModeGrepInput:
			m.items = m.filterViewed(msg.items)
			m.clampCursor()
		case ModePeek, ModePeekGrep:
			m.peekItems = msg.items
			m.clampPeekCursor()
		}
		return m, m.refreshPreviewCmd()

	case prLoadedMsg:
		m.prLoading = false
		if m.pr != nil {
			*m.pr = msg.pr
		}
		m.prComments = msg.comments
		m.prFullDiff = msg.fullDiff
		m.statusMsg = fmt.Sprintf("PR #%d: %s", msg.pr.Number, msg.pr.Title)
		items := prFilesToItems(msg.files, msg.fullDiff)
		cachedChangedItems = items
		m.items = items
		m.clampCursor()
		return m, m.refreshPreviewCmd()

	case publishDoneMsg:
		if msg.err != nil {
			m.statusMsg = "publish failed: " + msg.err.Error()
		} else {
			m.statusMsg = fmt.Sprintf("review published on PR #%d", m.pr.Number)
			m.notes = make(map[string]gh.Note)
		}
		return m, nil

	case previewReadyMsg:
		m.preview = msg.content
		m.previewLines = msg.lines
		return m, nil

	case editorDoneMsg:
		return m, m.refreshPreviewCmd()

	case errMsg:
		m.statusMsg = "error: " + msg.err.Error()
		return m, nil

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

// ── key handling ──────────────────────────────────────────────────────────────

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// overlays that capture all keys
	if m.mode == ModeHelp {
		m.mode = m.savedMode
		return m, nil
	}
	if m.mode == ModeNote {
		return m.handleNoteKey(msg)
	}
	if m.mode == ModeGrepInput {
		return m.handleGrepInputKey(msg)
	}
	if m.mode == ModePeek || m.mode == ModePeekGrep {
		return m.handlePeekKey(msg)
	}

	m.statusMsg = ""

	// preview-focused: j/k scroll, h returns, n notes at clicked line
	if m.focus == focusPreview {
		switch {
		case key.Matches(msg, keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, keys.Up):
			m.previewScrollOffset = clamp(m.previewScrollOffset-1, 0, len(m.previewLines))
			return m, nil
		case key.Matches(msg, keys.Down):
			m.previewScrollOffset = clamp(m.previewScrollOffset+1, 0, len(m.previewLines))
			return m, nil
		case key.Matches(msg, keys.FocusPrev), key.Matches(msg, keys.Esc):
			m.focus = focusList
			return m, nil
		case key.Matches(msg, keys.Note):
			return m.handleNoteOpen()
		}
		return m, nil
	}

	// list-focused: full key map
	switch {
	case key.Matches(msg, keys.Quit):
		return m, tea.Quit
	case key.Matches(msg, keys.Esc):
		return m.handleEsc()
	case key.Matches(msg, keys.Up):
		return m.moveCursor(-1)
	case key.Matches(msg, keys.Down):
		return m.moveCursor(1)
	case key.Matches(msg, keys.Enter):
		return m.handleEnter()
	case key.Matches(msg, keys.FocusNext):
		if m.layout != layoutHidden && !m.zoomed {
			m.focus = focusPreview
			m.previewScrollOffset = 0
		}
		return m, nil
	case key.Matches(msg, keys.Viewed):
		if m.mode != ModeNotesList && m.mode != ModeViewedList {
			return m.markViewed()
		}
	case key.Matches(msg, keys.ViewedList):
		return m.handleViewedList()
	case key.Matches(msg, keys.Toggle):
		m.full = !m.full
		return m, m.refreshPreviewCmd()
	case key.Matches(msg, keys.Refresh):
		if m.pr != nil {
			m.prLoading = true
		}
		return m, m.reloadListCmd()
	case key.Matches(msg, keys.Zoom):
		m.zoomed = !m.zoomed
		if m.zoomed {
			m.focus = focusPreview
		} else {
			m.focus = focusList
		}
		return m, nil
		return m, nil
	case key.Matches(msg, keys.Grep):
		return m.handleGrepOpen()
	case key.Matches(msg, keys.FileList):
		return m.handleFileList()
	case key.Matches(msg, keys.Peek):
		if m.mode != ModeNotesList && m.mode != ModeViewedList {
			return m.handlePeekOpen()
		}
	case key.Matches(msg, keys.Note):
		return m.handleNoteOpen()
	case key.Matches(msg, keys.NotesList):
		return m.handleNotesList()
	case key.Matches(msg, keys.Publish):
		if m.pr != nil && len(m.notes) > 0 {
			return m, publishNotes(m.pr, m.notesSlice(), m.prFullDiff)
		} else if m.pr != nil {
			m.statusMsg = "no notes to publish"
		} else {
			m.statusMsg = "not in PR mode (use --pr)"
		}
	case key.Matches(msg, keys.Help):
		m.savedMode = m.mode
		m.mode = ModeHelp
		return m, nil
	}
	return m, nil
}

func (m Model) handleGrepInputKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Esc):
		m.mode = ModeList
		m.query.SetValue("")
		m.query.Blur()
		return m, nil
	case key.Matches(msg, keys.GrepExec):
		q := m.query.Value()
		m.query.Blur()
		if q == "" {
			m.mode = ModeList
			return m, nil
		}
		m.mode = ModeGrep
		m.items = nil
		return m, grepChanged(m.root, m.since, q, m.cfg.Exclude, m.cfg.Sort)
	default:
		var cmd tea.Cmd
		m.query, cmd = m.query.Update(msg)
		return m, cmd
	}
}

func (m Model) handlePeekKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	m.statusMsg = ""
	switch {
	case key.Matches(msg, keys.Quit):
		return m, tea.Quit
	case key.Matches(msg, keys.Esc):
		return m.handleEsc()
	case key.Matches(msg, keys.Up):
		return m.moveCursor(-1)
	case key.Matches(msg, keys.Down):
		return m.moveCursor(1)
	case key.Matches(msg, keys.Enter):
		return m.handleEnter()
	case key.Matches(msg, keys.Refresh):
		return m, loadPeekItems(m.root)
	case key.Matches(msg, keys.FileList):
		return m.handleFileList()
	case key.Matches(msg, keys.Zoom):
		m.zoomed = !m.zoomed
		return m, nil
	case key.Matches(msg, keys.Grep):
		q := m.peekQuery.Value()
		m.peekItems = nil
		m.mode = ModePeekGrep
		return m, grepAll(m.root, q)
	default:
		var cmd tea.Cmd
		m.peekQuery, cmd = m.peekQuery.Update(msg)
		if m.mode == ModePeek {
			m.filterPeekByQuery()
			m.clampPeekCursor()
			return m, tea.Batch(cmd, m.refreshPreviewCmd())
		}
		return m, cmd
	}
}

// ── mouse handling ────────────────────────────────────────────────────────────

func (m Model) handleMouse(msg tea.MouseMsg) (Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}

	headerH := 1 // header is always 1 line
	footerH := m.footerHeight()
	usable := m.height - headerH - footerH

	x, y := msg.X, msg.Y
	contentY := y - headerH // row within the content area

	if contentY < 0 || contentY >= usable {
		return m, nil
	}

	listW := m.listWidth()
	previewX := listW + 1 // preview starts after list + divider

	// zoomed or list-hidden: entire screen is preview
	if m.zoomed || m.layout == layoutHidden {
		return m.handlePreviewClick(contentY)
	}

	if m.layout == layoutBottom {
		listH := usable * 40 / 100
		if contentY < listH {
			return m.handleListClick(contentY)
		}
		return m.handlePreviewClick(contentY - listH - 1)
	}

	if x < listW {
		return m.handleListClick(contentY)
	} else if x >= previewX {
		return m.handlePreviewClick(contentY)
	}

	return m, nil
}

func (m Model) handleListClick(row int) (Model, tea.Cmd) {
	items := m.activeItems()
	if len(items) == 0 {
		return m, nil
	}

	rows := buildTreeRows(items)
	cursor := m.activeCursor()

	// find which visual row the current cursor is on (for scroll offset)
	cursorRow := 0
	for ri, r := range rows {
		if r.kind == 1 && r.itemIndex == cursor {
			cursorRow = ri
			break
		}
	}
	startR, _ := scrollWindow(cursorRow, len(rows), m.listVisibleHeight())
	targetRow := startR + row

	if targetRow < 0 || targetRow >= len(rows) {
		return m, nil
	}

	r := rows[targetRow]
	if r.kind != 1 {
		return m, nil // clicked a dir header
	}

	// focus list, move cursor
	m.focus = focusList
	isPeek := m.mode == ModePeek || m.mode == ModePeekGrep
	if isPeek {
		m.peekCursor = r.itemIndex
	} else {
		m.cursor = r.itemIndex
	}
	return m, m.refreshPreviewCmd()
}

func (m Model) handlePreviewClick(row int) (Model, tea.Cmd) {
	m.focus = focusPreview

	// absolute index into previewLines
	lineIndex := m.previewScrollOffset + row
	if lineIndex < 0 || lineIndex >= len(m.previewLines) {
		return m, nil
	}

	// store visual row for reliable highlighting
	m.previewClickedRow = lineIndex

	// try to parse a file line number for annotation
	fileLine := extractLineNumber(m.previewLines[lineIndex])
	m.previewClickedLine = fileLine

	if fileLine > 0 {
		m.statusMsg = fmt.Sprintf("line %d selected — press n to annotate", fileLine)
	} else {
		m.statusMsg = "line selected — press n to annotate"
	}

	return m, nil
}

// extractLineNumber tries to parse a file line number from a rendered preview line.
// Handles bat format "  123 │ ..." and plain "  123  ..." and diff "@@ -N +N @@".
func extractLineNumber(line string) int {
	s := stripANSI(line)
	s = strings.TrimLeft(s, " ")

	// diff hunk header: @@ -old +new @@
	if strings.HasPrefix(s, "@@") {
		var old, new_ int
		fmt.Sscanf(s, "@@ -%d", &old)
		fmt.Sscanf(s, "@@ -%*d,%*d +%d", &new_)
		if new_ == 0 {
			fmt.Sscanf(s, "@@ -%*d +%d", &new_)
		}
		return new_
	}

	// bat/plain: "NNN │ ..." or "NNN  ..."
	// find first run of digits at start
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > 0 && i < len(s) {
		n := 0
		fmt.Sscanf(s[:i], "%d", &n)
		if n > 0 {
			return n
		}
	}

	return 0
}

// ── note handling ─────────────────────────────────────────────────────────────

func (m Model) handleNoteOpen() (Model, tea.Cmd) {
	item := m.selectedItem()
	if item == nil {
		return m, nil
	}
	m.notePath = item.Path
	// use clicked preview line if available (regardless of focus)
	if m.previewClickedRow >= 0 && m.previewClickedLine > 0 {
		m.noteLine = m.previewClickedLine
	} else if m.previewClickedRow >= 0 {
		// clicked but line number unknown — use row as approximation
		m.noteLine = m.previewClickedRow + 1
	} else {
		m.noteLine = item.Line
	}
	if existing, ok := m.notes[item.Path]; ok {
		m.noteTA.SetValue(existing.Body)
	} else {
		m.noteTA.SetValue("")
	}
	m.noteTA.Focus()
	m.savedMode = m.mode
	m.mode = ModeNote
	return m, textarea.Blink
}

func (m Model) handleNoteKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Submit):
		body := strings.TrimSpace(m.noteTA.Value())
		if body != "" {
			m.notes[m.notePath] = gh.Note{
				Path: m.notePath,
				Line: m.noteLine,
				Body: body,
			}
			m.statusMsg = fmt.Sprintf("note saved: %s", m.notePath)
		} else {
			delete(m.notes, m.notePath)
			m.statusMsg = fmt.Sprintf("note removed: %s", m.notePath)
		}
		m.mode = m.savedMode
		m.noteTA.Blur()
		return m, m.refreshPreviewCmd()
	case key.Matches(msg, keys.Esc):
		m.mode = m.savedMode
		m.noteTA.Blur()
		return m, nil
	}
	var cmd tea.Cmd
	m.noteTA, cmd = m.noteTA.Update(msg)
	return m, cmd
}

func (m Model) handleNotesList() (Model, tea.Cmd) {
	if m.mode == ModeNotesList {
		m.mode = ModeList
		return m, m.refreshPreviewCmd()
	}
	m.savedMode = m.mode
	m.mode = ModeNotesList
	return m, nil
}

func (m Model) notesSlice() []gh.Note {
	out := make([]gh.Note, 0, len(m.notes))
	for _, n := range m.notes {
		out = append(out, n)
	}
	return out
}

// ── viewed list ───────────────────────────────────────────────────────────────

func (m Model) handleViewedList() (Model, tea.Cmd) {
	if m.mode == ModeViewedList {
		m.mode = ModeList
		return m, m.refreshPreviewCmd()
	}
	m.savedMode = m.mode
	m.mode = ModeViewedList
	return m, nil
}

// unviewItem removes a path from the viewed map so it reappears in the list.
func (m Model) unviewItem(path string) (Model, tea.Cmd) {
	delete(m.viewed, path)
	m.statusMsg = fmt.Sprintf("unviewed: %s", path)
	// reload list so it reappears
	return m, m.reloadListCmd()
}

// ── navigation ────────────────────────────────────────────────────────────────

func (m Model) moveCursor(delta int) (Model, tea.Cmd) {
	isPeek := m.mode == ModePeek || m.mode == ModePeekGrep
	if isPeek {
		m.peekCursor = clamp(m.peekCursor+delta, 0, len(m.peekItems)-1)
	} else {
		m.cursor = clamp(m.cursor+delta, 0, len(m.items)-1)
	}
	// clear preview click state when file changes
	m.previewClickedRow = -1
	m.previewClickedLine = 0
	m.previewScrollOffset = 0
	return m, m.refreshPreviewCmd()
}

func (m Model) handleEsc() (Model, tea.Cmd) {
	switch m.mode {
	case ModePeek, ModePeekGrep:
		m.mode = ModeList
		m.cursor = m.savedListCursor
		m.items = m.savedListItems
		m.peekQuery.SetValue("")
		return m, m.refreshPreviewCmd()
	case ModeGrep:
		m.mode = ModeList
		m.query.SetValue("")
		return m, m.reloadListCmd()
	case ModeNotesList, ModeViewedList:
		m.mode = ModeList
		return m, m.refreshPreviewCmd()
	}
	if m.focus == focusPreview {
		m.focus = focusList
		return m, nil
	}
	return m, nil
}

func (m Model) handleEnter() (Model, tea.Cmd) {
	// in viewed list, enter = unview
	if m.mode == ModeViewedList {
		paths := m.viewedPaths()
		if m.cursor >= 0 && m.cursor < len(paths) {
			m2, cmd := m.unviewItem(paths[m.cursor])
			m2.mode = ModeList
			return m2, cmd
		}
		return m, nil
	}
	item := m.selectedItem()
	if item == nil {
		return m, nil
	}
	return m, openEditor(m.root, m.since, *item, m.cfg)
}

func (m Model) markViewed() (Model, tea.Cmd) {
	item := m.selectedItem()
	if item == nil || item.Status == git.StatusDeleted {
		return m, nil
	}
	hash := git.DiffHash(m.root, m.since, item.Path)
	m.viewed[item.Path] = hash
	newItems := make([]git.Item, 0, len(m.items)-1)
	for _, it := range m.items {
		if it.Path != item.Path {
			newItems = append(newItems, it)
		}
	}
	m.items = newItems
	m.clampCursor()
	m.statusMsg = fmt.Sprintf("marked viewed: %s", item.Path)
	return m, m.refreshPreviewCmd()
}

func (m Model) viewedPaths() []string {
	paths := make([]string, 0, len(m.viewed))
	for p := range m.viewed {
		paths = append(paths, p)
	}
	return paths
}

func (m Model) handleGrepOpen() (Model, tea.Cmd) {
	m.mode = ModeGrepInput
	m.query.SetValue("")
	m.query.Focus()
	return m, textinput.Blink
}

func (m Model) handleFileList() (Model, tea.Cmd) {
	if m.mode == ModePeek || m.mode == ModePeekGrep {
		m.mode = ModePeek
		m.peekQuery.SetValue("")
		return m, loadPeekItems(m.root)
	}
	m.mode = ModeList
	m.query.SetValue("")
	return m, m.reloadListCmd()
}

func (m Model) handlePeekOpen() (Model, tea.Cmd) {
	m.savedListCursor = m.cursor
	m.savedListItems = m.items
	m.mode = ModePeek
	m.peekCursor = 0
	m.peekQuery.SetValue("")
	return m, loadPeekItems(m.root)
}

// ── fuzzy filtering ───────────────────────────────────────────────────────────

var cachedChangedItems []git.Item
var cachedPeekItems []git.Item

func (m *Model) filterListByQuery() {
	q := m.query.Value()
	if q == "" {
		m.items = m.filterViewed(cachedChangedItems)
		return
	}
	paths := make([]string, len(cachedChangedItems))
	for i, it := range cachedChangedItems {
		paths[i] = it.Path
	}
	matches := fuzzy.Find(q, paths)
	filtered := make([]git.Item, 0, len(matches))
	for _, match := range matches {
		filtered = append(filtered, cachedChangedItems[match.Index])
	}
	m.items = m.filterViewed(filtered)
}

func (m *Model) filterPeekByQuery() {
	q := m.peekQuery.Value()
	if q == "" {
		m.peekItems = cachedPeekItems
		return
	}
	paths := make([]string, len(cachedPeekItems))
	for i, it := range cachedPeekItems {
		paths[i] = it.Path
	}
	matches := fuzzy.Find(q, paths)
	filtered := make([]git.Item, 0, len(matches))
	for _, match := range matches {
		filtered = append(filtered, cachedPeekItems[match.Index])
	}
	m.peekItems = filtered
}

func (m *Model) filterViewed(items []git.Item) []git.Item {
	if len(m.viewed) == 0 {
		return items
	}
	out := make([]git.Item, 0, len(items))
	for _, it := range items {
		if h, ok := m.viewed[it.Path]; ok {
			current := git.DiffHash(m.root, m.since, it.Path)
			if current == h {
				continue
			}
			delete(m.viewed, it.Path)
		}
		out = append(out, it)
	}
	return out
}

// ── async commands ────────────────────────────────────────────────────────────

func (m Model) reloadListCmd() tea.Cmd {
	if m.pr != nil {
		return loadPR(m.pr)
	}
	return loadItems(m.root, m.since, m.cfg.Exclude, m.cfg.Sort)
}

func loadItems(root, since string, exclude []string, sortOrder string) tea.Cmd {
	return func() tea.Msg {
		items, err := git.ChangedItems(root, since, exclude, sortOrder)
		if err != nil {
			return errMsg{err}
		}
		cachedChangedItems = items
		return itemsLoadedMsg{items}
	}
}

func loadPeekItems(root string) tea.Cmd {
	return func() tea.Msg {
		items, err := git.RepoFiles(root)
		if err != nil {
			return errMsg{err}
		}
		cachedPeekItems = items
		return itemsLoadedMsg{items}
	}
}

func grepChanged(root, since, query string, exclude []string, sortOrder string) tea.Cmd {
	return func() tea.Msg {
		items, err := git.GrepItems(root, since, query, exclude, sortOrder)
		if err != nil {
			return errMsg{err}
		}
		return itemsLoadedMsg{items}
	}
}

func grepAll(root, query string) tea.Cmd {
	return func() tea.Msg {
		items, err := git.GrepAll(root, query)
		if err != nil {
			return errMsg{err}
		}
		return itemsLoadedMsg{items}
	}
}

func loadPR(pr *gh.PR) tea.Cmd {
	return func() tea.Msg {
		if err := gh.Fetch(pr); err != nil {
			return errMsg{err}
		}
		comments, err := gh.Comments(*pr)
		if err != nil {
			return errMsg{err}
		}
		fullDiff, err := gh.Diff(*pr)
		if err != nil {
			return errMsg{err}
		}
		files, err := gh.Files(*pr)
		if err != nil {
			return errMsg{err}
		}
		return prLoadedMsg{pr: *pr, comments: comments, fullDiff: fullDiff, files: files}
	}
}

func prFilesToItems(files []string, fullDiff string) []git.Item {
	items := make([]git.Item, 0, len(files))
	for _, f := range files {
		if f == "" {
			continue
		}
		fileDiff := gh.FileDiff(fullDiff, f)
		st := git.StatusModified
		if strings.Contains(fileDiff, "\nnew file mode") {
			st = git.StatusAdded
		} else if strings.Contains(fileDiff, "\ndeleted file mode") {
			st = git.StatusDeleted
		} else if strings.Contains(fileDiff, "rename from ") {
			st = git.StatusRenamed
		}
		items = append(items, git.Item{Status: st, Path: f})
	}
	return items
}

func publishNotes(pr *gh.PR, notes []gh.Note, fullDiff string) tea.Cmd {
	return func() tea.Msg {
		err := gh.Publish(*pr, notes, fullDiff)
		return publishDoneMsg{err: err}
	}
}

func openEditor(root, since string, item git.Item, cfg config.Config) tea.Cmd {
	path := item.Path
	ln := item.Line
	if ln == 0 {
		ln = git.HunkLine(root, since, path)
	}
	if ln == 0 {
		ln = 1
	}
	ed := cfg.Editor
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if ed == "" {
		ed = "vim"
	}
	full := filepath.Join(root, path)
	var args []string
	if cfg.EditorLineFmt != "" {
		fmtStr := strings.ReplaceAll(cfg.EditorLineFmt, "{file}", full)
		fmtStr = strings.ReplaceAll(fmtStr, "{line}", fmt.Sprintf("%d", ln))
		args = strings.Fields(fmtStr)
	} else {
		base := filepath.Base(strings.Fields(ed)[0])
		switch base {
		case "vim", "nvim", "vi", "view":
			args = []string{fmt.Sprintf("+%d", ln), "--", full}
		case "code", "code-insiders", "codium", "cursor", "windsurf":
			args = []string{"-g", fmt.Sprintf("%s:%d", full, ln)}
		case "subl", "sublime_text", "hx", "helix":
			args = []string{fmt.Sprintf("%s:%d", full, ln)}
		case "emacs", "emacsclient", "nano", "micro":
			args = []string{fmt.Sprintf("+%d", ln), full}
		default:
			args = []string{full}
		}
	}
	edBin := strings.Fields(ed)[0]
	c := exec.Command(edBin, args...)
	c.Dir = root
	return tea.ExecProcess(c, func(err error) tea.Msg { return editorDoneMsg{} })
}

func (m Model) refreshPreviewCmd() tea.Cmd {
	item := m.selectedItem()
	if item == nil {
		return func() tea.Msg { return previewReadyMsg{} }
	}
	root := m.root
	since := m.since
	full := m.full
	w := m.previewWidth()
	it := *item
	comments := m.prComments
	prFullDiff := m.prFullDiff
	notes := m.notes
	isPR := m.pr != nil
	return func() tea.Msg {
		var content string
		if isPR && prFullDiff != "" {
			if full {
				// full-file mode: read actual file content
				content = renderPreview(root, since, it, true, w)
				// fall back to PR diff if file doesn't exist locally
				if content == "" {
					fileDiff := gh.FileDiff(prFullDiff, it.Path)
					content = renderDiffText(fileDiff, w)
				}
			} else {
				fileDiff := gh.FileDiff(prFullDiff, it.Path)
				if fileDiff != "" {
					content = renderDiffText(fileDiff, w)
				}
			}
		} else {
			content = renderPreview(root, since, it, full, w)
		}
		content = appendComments(content, it.Path, comments)
		content = appendNote(content, it.Path, notes)
		lines := strings.Split(content, "\n")
		return previewReadyMsg{content: content, lines: lines}
	}
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m Model) View() string {
	if m.width == 0 {
		return ""
	}
	if m.mode == ModeHelp {
		return m.renderHelp()
	}
	if m.mode == ModeNote {
		return m.renderNoteEditor()
	}
	if m.mode == ModeNotesList {
		return m.renderNotesList()
	}
	if m.mode == ModeViewedList {
		return m.renderViewedList()
	}

	header := m.renderHeader()
	footer := m.renderFooter()
	usable := m.height - lipgloss.Height(header) - lipgloss.Height(footer)
	if usable < 1 {
		usable = 1
	}

	// z zoomed: full-screen preview
	if m.zoomed {
		prev := m.renderPreviewPane(m.width, usable)
		return lipgloss.JoinVertical(lipgloss.Left, header, prev, footer)
	}

	switch m.layout {
	case layoutRight:
		return m.renderSideBySide(header, footer, usable)
	case layoutBottom:
		return m.renderTopBottom(header, footer, usable)
	case layoutHidden:
		return m.renderListOnly(header, footer, usable)
	}
	return ""
}

// ── viewed list overlay ───────────────────────────────────────────────────────

func (m Model) renderViewedList() string {
	header := m.renderHeader()
	footer := styleFooter.Width(m.width).Render("enter unview · esc back")

	usable := m.height - lipgloss.Height(header) - lipgloss.Height(footer)

	paths := m.viewedPaths()

	var sb strings.Builder
	if len(paths) == 0 {
		sb.WriteString(lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")).
			Render("  no viewed files"))
	} else {
		for i, p := range paths {
			prefix := "  "
			line := lipgloss.NewStyle().
				Foreground(lipgloss.Color("240")).
				Render(prefix+"● "+p)
			if i == m.cursor {
				line = styleSelected.Width(m.width).Render(prefix + "● " + p)
			}
			sb.WriteString(line + "\n")
		}
	}

	content := lipgloss.NewStyle().
		Width(m.width).Height(usable).
		Render(sb.String())

	return lipgloss.JoinVertical(lipgloss.Left, header, content, footer)
}

// ── help popup ────────────────────────────────────────────────────────────────

var styleOverlay = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(lipgloss.Color("99")).
	Padding(1, 2)

const helpText = `
 wake — review a changeset you didn't write

 ── Navigation ───────────────────────────────────────────
  j / ↓      move down
  k / ↑      move up
  enter      open file in $EDITOR at first changed hunk
  q          quit

 ── Panes ────────────────────────────────────────────────
  l          focus preview pane (j/k to scroll)
  h / esc    focus back to file list
  click      click file in list to select · click line in preview to select it

 ── View ─────────────────────────────────────────────────
  t          toggle diff / whole-file view
  z          zoom preview full-screen (toggle)

 ── Search ───────────────────────────────────────────────
  /          grep changed files  (enter to search, esc cancel)
  f          back to file list
  p          peek: fuzzy-find any repo file (type to filter)
  r          refresh

 ── Review ───────────────────────────────────────────────
  v          mark file viewed — hides it until its diff changes
  V          show viewed files (enter to unview)
  n          add / edit a note on current file
             (click a line in preview first to annotate that line)
  N          view all pending notes
{{PR_PUBLISH}}

 ── Invocation ───────────────────────────────────────────
  wake                       local changes vs HEAD
  wake --since main          everything on this branch
  wake --pr 42               review GitHub PR #42
  wake --pr <url>            review by full GitHub URL

 ── Config  ~/.config/wake/config.toml or .wake.toml ─────
  editor, preview, preview_width, sort, exclude, since

 press any key to close
`

func (m Model) renderHelp() string {
	text := strings.TrimPrefix(helpText, "\n")
	if m.pr != nil {
		text = strings.ReplaceAll(text, "{{PR_PUBLISH}}", "  P          publish notes as GitHub PR review")
	} else {
		text = strings.ReplaceAll(text, "{{PR_PUBLISH}}", "")
	}
	lines := strings.Split(text, "\n")
	styleTitle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99"))
	styleSect := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("33"))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleKey := lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	styleNormal := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))

	var rendered []string
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, " wake —"):
			rendered = append(rendered, styleTitle.Render(line))
		case strings.HasPrefix(line, " ──"):
			rendered = append(rendered, styleSect.Render(line))
		case strings.HasPrefix(line, "  ") && strings.Contains(line, "  "):
			parts := strings.SplitN(strings.TrimLeft(line, " "), "  ", 2)
			indent := strings.Repeat(" ", len(line)-len(strings.TrimLeft(line, " ")))
			if len(parts) == 2 {
				rendered = append(rendered, indent+styleKey.Render(parts[0])+"  "+styleDim.Render(strings.TrimLeft(parts[1], " ")))
			} else {
				rendered = append(rendered, styleDim.Render(line))
			}
		case strings.TrimSpace(line) == "press any key to close":
			rendered = append(rendered, styleDim.Render(line))
		default:
			rendered = append(rendered, styleNormal.Render(line))
		}
	}

	content := strings.Join(rendered, "\n")
	box := styleOverlay.Render(content)
	bw := lipgloss.Width(box)
	bh := lipgloss.Height(box)
	padL := (m.width - bw) / 2
	padT := (m.height - bh) / 2
	if padL < 0 {
		padL = 0
	}
	if padT < 0 {
		padT = 0
	}
	var sb strings.Builder
	sb.WriteString(strings.Repeat("\n", padT))
	left := strings.Repeat(" ", padL)
	for _, line := range strings.Split(box, "\n") {
		sb.WriteString(left + line + "\n")
	}
	return sb.String()
}

// ── note editor overlay ───────────────────────────────────────────────────────

func (m Model) renderNoteEditor() string {
	title := fmt.Sprintf(" note: %s ", m.notePath)
	if m.noteLine > 0 {
		title = fmt.Sprintf(" note: %s:%d ", m.notePath, m.noteLine)
	}
	hint := styleFooter.Render("ctrl-s save · esc cancel")
	box := styleOverlay.Render(
		lipgloss.JoinVertical(lipgloss.Left,
			lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99")).Render(title),
			"",
			m.noteTA.View(),
			"",
			hint,
		),
	)
	bw := lipgloss.Width(box)
	bh := lipgloss.Height(box)
	padL := (m.width - bw) / 2
	padT := (m.height - bh) / 2
	if padL < 0 {
		padL = 0
	}
	if padT < 0 {
		padT = 0
	}
	var sb strings.Builder
	sb.WriteString(strings.Repeat("\n", padT))
	left := strings.Repeat(" ", padL)
	for _, line := range strings.Split(box, "\n") {
		sb.WriteString(left + line + "\n")
	}
	return sb.String()
}

// ── notes list overlay ────────────────────────────────────────────────────────

func (m Model) renderNotesList() string {
	header := m.renderHeader()
	hintStr := "n edit · esc back"
	if m.pr != nil {
		hintStr += " · P publish"
	}
	footer := styleFooter.Width(m.width).Render(hintStr)
	usable := m.height - lipgloss.Height(header) - lipgloss.Height(footer)

	var sb strings.Builder
	if len(m.notes) == 0 {
		sb.WriteString(lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")).
			Render("  no notes yet — press n on any file"))
	} else {
		for path, note := range m.notes {
			sb.WriteString(lipgloss.NewStyle().Bold(true).
				Foreground(lipgloss.Color("99")).Render("  ● " + path))
			if note.Line > 0 {
				sb.WriteString(lipgloss.NewStyle().
					Foreground(lipgloss.Color("240")).
					Render(fmt.Sprintf(":%d", note.Line)))
			}
			sb.WriteByte('\n')
			for _, line := range strings.Split(note.Body, "\n") {
				sb.WriteString("    " + line + "\n")
			}
			sb.WriteByte('\n')
		}
	}
	content := lipgloss.NewStyle().Width(m.width).Height(usable).Render(sb.String())
	return lipgloss.JoinVertical(lipgloss.Left, header, content, footer)
}

// ── list pane ─────────────────────────────────────────────────────────────────

var (
	styleSelected = lipgloss.NewStyle().Background(lipgloss.Color("237")).Bold(true)
	styleStatusM  = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	styleStatusA  = lipgloss.NewStyle().Foreground(lipgloss.Color("76")).Bold(true)
	styleStatusD  = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	styleStatusR  = lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Bold(true)
	styleStatusG  = lipgloss.NewStyle().Foreground(lipgloss.Color("165")).Bold(true)
	styleStatusP  = lipgloss.NewStyle().Foreground(lipgloss.Color("44")).Bold(true)
	styleNote     = lipgloss.NewStyle().Foreground(lipgloss.Color("99")).Bold(true)
)

func (m Model) renderSideBySide(header, footer string, usable int) string {
	listW := m.listWidth()
	prevW := m.width - listW - 1

	divColor := lipgloss.Color("240")
	if m.focus == focusPreview {
		divColor = lipgloss.Color("99")
	}
	divStyle := lipgloss.NewStyle().Width(1).Height(usable).Foreground(divColor)

	list := m.renderList(listW, usable)
	prev := m.renderPreviewPane(prevW, usable)

	row := lipgloss.JoinHorizontal(lipgloss.Top, list, divStyle.Render("│"), prev)
	return lipgloss.JoinVertical(lipgloss.Left, header, row, footer)
}

func (m Model) renderTopBottom(header, footer string, usable int) string {
	listH := usable * 40 / 100
	prevH := usable - listH - 1
	if prevH < 1 {
		prevH = 1
	}
	list := m.renderList(m.width, listH)
	prev := m.renderPreviewPane(m.width, prevH)
	div := strings.Repeat("─", m.width)
	return lipgloss.JoinVertical(lipgloss.Left, header, list, div, prev, footer)
}

func (m Model) renderListOnly(header, footer string, usable int) string {
	list := m.renderList(m.width, usable)
	return lipgloss.JoinVertical(lipgloss.Left, header, list, footer)
}

func (m Model) renderList(width, height int) string {
	items := m.activeItems()
	cursor := m.activeCursor()

	if len(items) == 0 {
		msg := "  nothing changed"
		if m.prLoading {
			msg = "  loading PR…"
		}
		return lipgloss.NewStyle().
			Width(width).Height(height).
			Foreground(lipgloss.Color("240")).
			Render(msg)
	}

	rows := buildTreeRows(items)

	cursorRow := 0
	for ri, r := range rows {
		if r.kind == 1 && r.itemIndex == cursor {
			cursorRow = ri
			break
		}
	}

	startR, endR := scrollWindow(cursorRow, len(rows), height)

	styleDirHeader := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleFocused := lipgloss.NewStyle().Background(lipgloss.Color("237")).Bold(true)
	_ = styleFocused

	var sb strings.Builder
	for ri := startR; ri < endR; ri++ {
		r := rows[ri]
		var line string

		if r.kind == 0 {
			line = styleDirHeader.Render("  " + r.dir + "/")
			line = padRight(line, width)
		} else {
			it := items[r.itemIndex]
			st := statusStyle(it.Status).Render(it.Status)

			noteMark := ""
			if _, hasNote := m.notes[it.Path]; hasNote {
				noteMark = styleNote.Render("●") + " "
			}

			var label string
			if it.Status == git.StatusGrep {
				label = fmt.Sprintf("%s:%d  %s", it.Path, it.Line, truncate(it.Text, width-20))
			} else {
				label = filepath.Base(it.Path)
			}

			indent := "  "
			if strings.Contains(it.Path, "/") {
				indent = "    "
			}

			line = fmt.Sprintf("%s%s  %s%s", indent, st, noteMark, label)
			line = truncate(line, width)
			line = padRight(line, width)

			if r.itemIndex == cursor {
				line = styleSelected.Width(width).Render(line)
			}
		}

		sb.WriteString(line)
		sb.WriteByte('\n')
	}

	rendered := sb.String()
	lines := strings.Count(rendered, "\n")
	for lines < height {
		rendered += strings.Repeat(" ", width) + "\n"
		lines++
	}
	return rendered
}

// ── preview pane ──────────────────────────────────────────────────────────────

func (m Model) renderPreviewPane(width, height int) string {
	lines := m.previewLines
	if len(lines) == 0 {
		lines = strings.Split(m.preview, "\n")
	}

	// apply scroll offset
	start := m.previewScrollOffset
	if start > len(lines) {
		start = len(lines)
	}
	visible := lines[start:]
	if len(visible) > height {
		visible = visible[:height]
	}

	styleClickedLine := lipgloss.NewStyle().
		Background(lipgloss.Color("57")).
		Foreground(lipgloss.Color("255")).
		Bold(true)

	var sb strings.Builder
	for i, line := range visible {
		absoluteI := start + i
		// highlight by visual row index — reliable regardless of line format
		if m.previewClickedRow >= 0 && absoluteI == m.previewClickedRow {
			line = styleClickedLine.Render(padRight(stripANSI(line), width))
			sb.WriteString(line)
			sb.WriteByte('\n')
			continue
		}
		visible2 := stripANSI(line)
		if len(visible2) > width {
			line = truncateANSI(line, width)
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}

	rendered := sb.String()
	lc := strings.Count(rendered, "\n")
	for lc < height {
		rendered += "\n"
		lc++
	}
	return rendered
}

func appendComments(preview, path string, comments []gh.ReviewComment) string {
	var relevant []gh.ReviewComment
	for _, c := range comments {
		if c.Path == path {
			relevant = append(relevant, c)
		}
	}
	if len(relevant) == 0 {
		return preview
	}
	var sb strings.Builder
	sb.WriteString(preview)
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(strings.Repeat("─", 40)))
	sb.WriteString("\n")
	for _, c := range relevant {
		h := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("33")).Render(fmt.Sprintf("  @%s", c.Author))
		if c.Line > 0 {
			h += lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(fmt.Sprintf(" line %d", c.Line))
		}
		sb.WriteString(h + "\n")
		for _, line := range strings.Split(c.Body, "\n") {
			sb.WriteString("  " + line + "\n")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func appendNote(preview, path string, notes map[string]gh.Note) string {
	note, ok := notes[path]
	if !ok {
		return preview
	}
	var sb strings.Builder
	sb.WriteString(preview)
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("99")).Render(strings.Repeat("─", 40)))
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99")).Render("  ● your note") + "\n")
	for _, line := range strings.Split(note.Body, "\n") {
		sb.WriteString("  " + line + "\n")
	}
	return sb.String()
}

// ── header / footer ───────────────────────────────────────────────────────────

var (
	styleHeader = lipgloss.NewStyle().
			Background(lipgloss.Color("235")).
			Foreground(lipgloss.Color("252")).
			Padding(0, 1)
	styleFooter = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")).
			Padding(0, 1)
	styleBrand = lipgloss.NewStyle().
			Foreground(lipgloss.Color("99")).
			Bold(true)
)

func (m Model) renderHeader() string {
	prompt := m.promptLabel()
	query := m.activeQuery()

	prLabel := ""
	if m.pr != nil {
		title := m.pr.Title
		if title == "" {
			title = fmt.Sprintf("PR #%d", m.pr.Number)
		} else {
			title = fmt.Sprintf("PR #%d: %s", m.pr.Number, title)
		}
		prLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("33")).
			Render(" [" + truncate(title, 50) + "]")
	}

	noteBadge := ""
	if len(m.notes) > 0 {
		noteBadge = lipgloss.NewStyle().Foreground(lipgloss.Color("99")).Bold(true).
			Render(fmt.Sprintf(" ●%d", len(m.notes)))
	}

	viewedBadge := ""
	if len(m.viewed) > 0 {
		viewedBadge = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).
			Render(fmt.Sprintf(" ✓%d", len(m.viewed)))
	}

	// pane focus indicator
	focusIndicator := ""
	if m.zoomed {
		focusIndicator = lipgloss.NewStyle().Foreground(lipgloss.Color("99")).Bold(true).Render(" [zoom]")
	} else if m.focus == focusPreview {
		focusIndicator = lipgloss.NewStyle().Foreground(lipgloss.Color("99")).Render(" [preview]")
	}

	count := fmt.Sprintf("%d/%d", m.activeCursor()+1, len(m.activeItems()))
	if len(m.activeItems()) == 0 {
		count = "0/0"
	}
	brand := styleBrand.Render("wake")
	left := fmt.Sprintf("%s  %s%s%s", prompt, query, prLabel, focusIndicator)
	right := fmt.Sprintf("%s%s%s  %s", count, noteBadge, viewedBadge, brand)
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}
	line := left + strings.Repeat(" ", gap) + right
	return styleHeader.Width(m.width).Render(line)
}

func (m Model) renderFooter() string {
	var hints string
	switch m.mode {
	case ModeList:
		toggle := "diff/full"
		if m.full {
			toggle = "full/diff"
		}
		if m.zoomed {
			noteHint := "n note"
			if m.previewClickedLine > 0 {
				noteHint = fmt.Sprintf("n note@line%d", m.previewClickedLine)
			} else if m.previewClickedRow >= 0 {
				noteHint = fmt.Sprintf("n note@row%d", m.previewClickedRow+1)
			}
			hints = fmt.Sprintf("j/k scroll · %s · z unzoom", noteHint)
		} else if m.focus == focusPreview {
			noteHint := "n note"
			if m.previewClickedLine > 0 {
				noteHint = fmt.Sprintf("n note@line%d", m.previewClickedLine)
			} else if m.previewClickedRow >= 0 {
				noteHint = fmt.Sprintf("n note@row%d", m.previewClickedRow+1)
			}
			hints = fmt.Sprintf("j/k scroll · %s · h back to list", noteHint)
		} else {
			base := fmt.Sprintf("enter open · v viewed · V viewed-list · t %s · / grep · p peek · l preview · z zoom · n note · N notes · r refresh · H help", toggle)
			if m.pr != nil {
				base += " · P publish"
			}
			hints = base
		}
	case ModeGrep:
		hints = "enter open · v viewed · n note · f file list · r refresh · H help"
	case ModeGrepInput:
		hints = "enter search · esc cancel"
	case ModePeek:
		hints = "enter open · type to filter · / grep repo · f file list · esc back"
	case ModePeekGrep:
		hints = "enter open · f file list · esc back"
	case ModeNotesList:
		hints = "n edit · esc back"
		if m.pr != nil {
			hints += " · P publish"
		}
	case ModeViewedList:
		hints = "enter unview · esc back"
	case ModeHelp:
		hints = "any key to close"
	}

	if m.statusMsg != "" {
		msg := lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true).Render(m.statusMsg + "  ")
		return lipgloss.JoinVertical(lipgloss.Left,
			styleFooter.Width(m.width).Render(msg),
			styleFooter.Width(m.width).Render(hints),
		)
	}
	return styleFooter.Width(m.width).Render(hints)
}

func (m Model) promptLabel() string {
	switch m.mode {
	case ModeGrepInput:
		return "grep/"
	case ModeGrep:
		return "grep>"
	case ModePeek:
		return "peek>"
	case ModePeekGrep:
		return "repo-grep>"
	case ModeNotesList:
		return "notes>"
	case ModeViewedList:
		return "viewed>"
	case ModeHelp:
		return "help"
	default:
		return "changed>"
	}
}

// ── geometry helpers ──────────────────────────────────────────────────────────

func (m Model) footerHeight() int {
	if m.statusMsg != "" {
		return 2
	}
	return 1
}

func (m Model) listVisibleHeight() int {
	headerH := 1
	footerH := m.footerHeight()
	usable := m.height - headerH - footerH
	if m.layout == layoutBottom {
		return usable * 40 / 100
	}
	return usable
}

func (m Model) previewWidth() int {
	w := m.width * m.cfg.PreviewWidth / 100
	if w < 20 {
		w = 20
	}
	return w
}

func (m Model) listWidth() int {
	return m.width - m.previewWidth() - 1
}

// ── helpers ───────────────────────────────────────────────────────────────────

func (m Model) selectedItem() *git.Item {
	items := m.activeItems()
	cursor := m.activeCursor()
	if len(items) == 0 || cursor >= len(items) {
		return nil
	}
	it := items[cursor]
	return &it
}

func (m Model) activeItems() []git.Item {
	if m.mode == ModePeek || m.mode == ModePeekGrep {
		return m.peekItems
	}
	return m.items
}

func (m Model) activeCursor() int {
	if m.mode == ModePeek || m.mode == ModePeekGrep {
		return m.peekCursor
	}
	return m.cursor
}

func (m Model) activeQuery() string {
	if m.mode == ModePeek || m.mode == ModePeekGrep {
		return m.peekQuery.Value()
	}
	return m.query.Value()
}

func (m *Model) clampCursor() {
	m.cursor = clamp(m.cursor, 0, len(m.items)-1)
}

func (m *Model) clampPeekCursor() {
	m.peekCursor = clamp(m.peekCursor, 0, len(m.peekItems)-1)
}

func statusStyle(s string) lipgloss.Style {
	switch s {
	case git.StatusModified:
		return styleStatusM
	case git.StatusAdded:
		return styleStatusA
	case git.StatusDeleted:
		return styleStatusD
	case git.StatusRenamed:
		return styleStatusR
	case git.StatusGrep:
		return styleStatusG
	case git.StatusPeek:
		return styleStatusP
	}
	return lipgloss.NewStyle()
}

func buildTreeRows(items []git.Item) []struct {
	kind      int // 0=dir header, 1=item
	itemIndex int
	dir       string
} {
	type row = struct {
		kind      int
		itemIndex int
		dir       string
	}
	var rows []row
	lastDir := "\x00"
	for i, it := range items {
		dir := filepath.Dir(it.Path)
		if dir == "." {
			dir = ""
		}
		if dir != lastDir {
			if dir != "" {
				rows = append(rows, row{kind: 0, dir: dir})
			}
			lastDir = dir
		}
		rows = append(rows, row{kind: 1, itemIndex: i})
	}
	return rows
}

func layoutFromConfig(s string) previewLayout {
	switch s {
	case "bottom":
		return layoutBottom
	case "hidden":
		return layoutHidden
	default:
		return layoutRight
	}
}

func scrollWindow(cursor, total, height int) (start, end int) {
	if total <= height {
		return 0, total
	}
	start = cursor - height/2
	if start < 0 {
		start = 0
	}
	end = start + height
	if end > total {
		end = total
		start = end - height
		if start < 0 {
			start = 0
		}
	}
	return start, end
}

func clamp(v, lo, hi int) int {
	if hi < 0 {
		return 0
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func padRight(s string, width int) string {
	vis := len([]rune(stripANSI(s)))
	if vis >= width {
		return s
	}
	return s + strings.Repeat(" ", width-vis)
}

func stripANSI(s string) string {
	var out strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\033' && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

func truncateANSI(s string, max int) string {
	var out strings.Builder
	visible := 0
	i := 0
	for i < len(s) && visible < max {
		if s[i] == '\033' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			j++
			out.WriteString(s[i:j])
			i = j
			continue
		}
		out.WriteByte(s[i])
		visible++
		i++
	}
	return out.String()
}
