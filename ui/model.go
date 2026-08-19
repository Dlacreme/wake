package ui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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
	ModeNote            // note editor (new thread first message)
	ModeReply           // reply editor (add message to existing thread)
	ModeNotesList       // all threads overview
	ModeViewedList      // viewed files
	ModeHelp            // help popup
)

type focusPane int

const (
	focusList    focusPane = iota
	focusPreview focusPane = iota
)

// ── messages ──────────────────────────────────────────────────────────────────

type itemsLoadedMsg struct{ items []git.Item }
type previewReadyMsg struct {
	content string
	lines   []string
}
type editorDoneMsg struct{}
type errMsg struct{ err error }
type prLoadedMsg struct {
	pr       gh.PR
	threads  map[string]*gh.NoteThread
	fullDiff string
	files    []string
}
type publishDoneMsg struct{ err error }

// ── model ─────────────────────────────────────────────────────────────────────

type Model struct {
	mode  Mode
	focus focusPane

	items  []git.Item
	cursor int
	query  textinput.Model

	viewed map[string]string

	preview             string
	previewLines        []string
	previewScrollOffset int
	previewClickedRow   int
	previewClickedLine  int
	layout              previewLayout
	zoomed              bool
	full                bool

	peekItems       []git.Item
	peekCursor      int
	peekQuery       textinput.Model
	savedListCursor int
	savedListItems  []git.Item
	savedMode       Mode

	width  int
	height int

	cfg   config.Config
	root  string
	since string

	pr           *gh.PR
	prFullDiff   string
	prLoading    bool
	prRepoMatch  bool // true when PR repo matches local git remote

	// threads: key = gh.ThreadKey(path, line)
	threads   map[string]*gh.NoteThread
	noteTA    textarea.Model
	notePath  string
	noteLine  int
	replyKey  string // thread key being replied to

	statusMsg string
}

// ── constructor ───────────────────────────────────────────────────────────────

func New(root, since string, cfg config.Config, pr *gh.PR, prRepoMatch bool) Model {
	qi := textinput.New()
	qi.Placeholder = ""
	qi.Prompt = ""

	pi := textinput.New()
	pi.Placeholder = ""
	pi.Prompt = ""

	ta := textarea.New()
	ta.Placeholder = "Write your message… (ctrl-s to save, esc to cancel)"
	ta.ShowLineNumbers = false
	ta.SetWidth(80)
	ta.SetHeight(8)

	return Model{
		mode:              ModeList,
		focus:             focusList,
		viewed:            make(map[string]string),
		threads:           make(map[string]*gh.NoteThread),
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
		prRepoMatch:       prRepoMatch,
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
		m.prFullDiff = msg.fullDiff
		m.statusMsg = fmt.Sprintf("PR #%d: %s", msg.pr.Number, msg.pr.Title)
		// merge GH threads (don't overwrite local ones)
		for k, t := range msg.threads {
			if existing, ok := m.threads[k]; ok {
				// prepend GH messages before local ones
				existing.Messages = append(t.Messages, existing.Messages...)
				existing.DiffPos = t.DiffPos
			} else {
				m.threads[k] = t
			}
		}
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
			// clear local messages, keep GH ones
			for _, t := range m.threads {
				var ghMsgs []gh.Message
				for _, msg := range t.Messages {
					if msg.FromGH {
						ghMsgs = append(ghMsgs, msg)
					}
				}
				t.Messages = ghMsgs
			}
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
	if m.mode == ModeHelp {
		m.mode = m.savedMode
		return m, nil
	}
	if m.mode == ModeNote || m.mode == ModeReply {
		return m.handleNoteKey(msg)
	}
	if m.mode == ModeGrepInput {
		return m.handleGrepInputKey(msg)
	}
	if m.mode == ModePeek || m.mode == ModePeekGrep {
		return m.handlePeekKey(msg)
	}

	m.statusMsg = ""

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
		case key.Matches(msg, keys.Reply):
			return m.handleReplyOpen()
		}
		return m, nil
	}

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
	case key.Matches(msg, keys.Zoom):
		m.zoomed = !m.zoomed
		if m.zoomed {
			m.focus = focusPreview
		} else {
			m.focus = focusList
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
	case key.Matches(msg, keys.Reply):
		return m.handleReplyOpen()
	case key.Matches(msg, keys.NotesList):
		return m.handleNotesList()
	case key.Matches(msg, keys.Publish):
		if m.pr != nil {
			if m.hasLocalThreads() {
				return m, publishThreads(m.pr, m.threads, m.prFullDiff)
			}
			m.statusMsg = "no local notes to publish"
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
	headerH := 1
	footerH := m.footerHeight()
	usable := m.height - headerH - footerH
	x, y := msg.X, msg.Y
	contentY := y - headerH
	if contentY < 0 || contentY >= usable {
		return m, nil
	}
	listW := m.listWidth()
	previewX := listW + 1

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
		return m, nil
	}
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
	lineIndex := m.previewScrollOffset + row
	if lineIndex < 0 || lineIndex >= len(m.previewLines) {
		return m, nil
	}
	// skip injected thread block lines (sentinel prefix)
	if strings.HasPrefix(m.previewLines[lineIndex], threadSentinel) {
		return m, nil
	}
	m.previewClickedRow = lineIndex
	fileLine := extractLineNumber(m.previewLines[lineIndex])
	m.previewClickedLine = fileLine
	if fileLine > 0 {
		m.statusMsg = fmt.Sprintf("line %d — n to annotate · R to reply", fileLine)
	} else {
		m.statusMsg = "line selected — n to annotate · R to reply"
	}
	return m, nil
}

// ── note / reply handling ─────────────────────────────────────────────────────

func (m Model) handleNoteOpen() (Model, tea.Cmd) {
	item := m.selectedItem()
	if item == nil {
		return m, nil
	}
	path := item.Path
	line := m.effectiveLine(item)
	key := gh.ThreadKey(path, line)

	// if thread exists and visible → toggle visibility
	if t, ok := m.threads[key]; ok && len(t.Messages) > 0 {
		t.Visible = !t.Visible
		return m, m.refreshPreviewCmd()
	}

	// no thread → open editor to create first message
	m.notePath = path
	m.noteLine = line
	m.noteTA.SetValue("")
	m.noteTA.Focus()
	m.savedMode = m.mode
	m.mode = ModeNote
	return m, textarea.Blink
}

func (m Model) handleReplyOpen() (Model, tea.Cmd) {
	item := m.selectedItem()
	if item == nil {
		return m, nil
	}
	path := item.Path
	line := m.effectiveLine(item)
	key := gh.ThreadKey(path, line)

	m.replyKey = key
	m.notePath = path
	m.noteLine = line
	m.noteTA.SetValue("")
	m.noteTA.Focus()
	m.savedMode = m.mode
	m.mode = ModeReply
	return m, textarea.Blink
}

func (m Model) handleNoteKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Submit):
		body := strings.TrimSpace(m.noteTA.Value())
		m.noteTA.Blur()
		m.mode = m.savedMode
		if body == "" {
			return m, nil
		}
		msg := gh.Message{
			Author:    "you",
			Body:      body,
			CreatedAt: time.Now(),
			FromGH:    false,
		}
		tkey := gh.ThreadKey(m.notePath, m.noteLine)
		if t, ok := m.threads[tkey]; ok {
			t.Messages = append(t.Messages, msg)
		} else {
			m.threads[tkey] = &gh.NoteThread{
				Path:    m.notePath,
				Line:    m.noteLine,
				Visible: true,
				Messages: []gh.Message{msg},
			}
		}
		m.threads[tkey].Visible = true
		m.statusMsg = fmt.Sprintf("note added: %s", m.notePath)
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

// effectiveLine returns the line to use for a note:
// previewClickedLine if a preview line was clicked, else item.Line.
func (m Model) effectiveLine(item *git.Item) int {
	if m.previewClickedRow >= 0 && m.previewClickedLine > 0 {
		return m.previewClickedLine
	}
	if m.previewClickedRow >= 0 {
		return m.previewClickedRow + 1
	}
	return item.Line
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

func (m Model) hasLocalThreads() bool {
	for _, t := range m.threads {
		if t.HasLocal() {
			return true
		}
	}
	return false
}

func (m Model) threadCount() int {
	n := 0
	for _, t := range m.threads {
		if len(t.Messages) > 0 {
			n++
		}
	}
	return n
}

// ── viewed ────────────────────────────────────────────────────────────────────

func (m Model) handleViewedList() (Model, tea.Cmd) {
	if m.mode == ModeViewedList {
		m.mode = ModeList
		return m, m.refreshPreviewCmd()
	}
	m.savedMode = m.mode
	m.mode = ModeViewedList
	return m, nil
}

func (m Model) unviewItem(path string) (Model, tea.Cmd) {
	delete(m.viewed, path)
	m.statusMsg = fmt.Sprintf("unviewed: %s", path)
	return m, m.reloadListCmd()
}

func (m Model) viewedPaths() []string {
	paths := make([]string, 0, len(m.viewed))
	for p := range m.viewed {
		paths = append(paths, p)
	}
	return paths
}

// ── navigation ────────────────────────────────────────────────────────────────

func (m Model) moveCursor(delta int) (Model, tea.Cmd) {
	isPeek := m.mode == ModePeek || m.mode == ModePeekGrep
	if isPeek {
		m.peekCursor = clamp(m.peekCursor+delta, 0, len(m.peekItems)-1)
	} else {
		m.cursor = clamp(m.cursor+delta, 0, len(m.items)-1)
	}
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
	if m.mode == ModeViewedList {
		paths := m.viewedPaths()
		if m.cursor >= 0 && m.cursor < len(paths) {
			m2, cmd := m.unviewItem(paths[m.cursor])
			m2.mode = ModeList
			return m2, cmd
		}
		return m, nil
	}
	// can't open editor for a PR from a different repo
	if m.pr != nil && !m.prRepoMatch {
		m.statusMsg = "editor unavailable — PR is from a different repository"
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
		threads, err := gh.Comments(*pr)
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
		return prLoadedMsg{pr: *pr, threads: threads, fullDiff: fullDiff, files: files}
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

func publishThreads(pr *gh.PR, threads map[string]*gh.NoteThread, fullDiff string) tea.Cmd {
	return func() tea.Msg {
		err := gh.Publish(*pr, threads, fullDiff)
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

// ── preview with inline threads ───────────────────────────────────────────────

// threadSentinel prefixes every injected thread line so we can skip them
// during click hit-testing and line-number extraction.
const threadSentinel = "\x00thread\x00"

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
	prFullDiff := m.prFullDiff
	isPR := m.pr != nil
	prRepoMatch := m.prRepoMatch
	threads := m.threads
	return func() tea.Msg {
		var content string
		if isPR && prFullDiff != "" {
			if full && prRepoMatch {
				// full-file only works when files exist locally
				content = renderPreview(root, since, it, true, w)
				if content == "" {
					fileDiff := gh.FileDiff(prFullDiff, it.Path)
					content = renderDiffText(fileDiff, w)
				}
			} else if full && !prRepoMatch {
				// can't read local files — fall back to PR diff with a notice
				fileDiff := gh.FileDiff(prFullDiff, it.Path)
				notice := lipgloss.NewStyle().
					Foreground(lipgloss.Color("214")).Bold(true).
					Render("  ⚠  full-file view unavailable (PR from a different repository)") + "\n\n"
				content = notice + renderDiffText(fileDiff, w)
			} else {
				fileDiff := gh.FileDiff(prFullDiff, it.Path)
				if fileDiff != "" {
					content = renderDiffText(fileDiff, w)
				}
			}
		} else {
			content = renderPreview(root, since, it, full, w)
		}
		// inject inline thread blocks
		lines := strings.Split(content, "\n")
		lines = injectThreads(lines, it.Path, threads, w)
		return previewReadyMsg{
			content: strings.Join(lines, "\n"),
			lines:   lines,
		}
	}
}

// injectThreads inserts thread blocks after the line they're anchored to.
// File-level threads (Line=0) are prepended at the top.
func injectThreads(lines []string, path string, threads map[string]*gh.NoteThread, width int) []string {
	// collect threads for this file
	var fileLevelThreads []*gh.NoteThread
	lineThreads := make(map[int]*gh.NoteThread)
	for _, t := range threads {
		if t.Path != path || !t.Visible || len(t.Messages) == 0 {
			continue
		}
		if t.Line == 0 {
			fileLevelThreads = append(fileLevelThreads, t)
		} else {
			lineThreads[t.Line] = t
		}
	}

	var out []string

	// file-level threads at top
	for _, t := range fileLevelThreads {
		out = append(out, renderThreadBlock(t, width)...)
	}

	// track current file line as we scan (counting non-deleted diff lines)
	currentLine := 0
	for _, line := range lines {
		plain := stripANSI(line)
		// track line numbers from diff output
		if strings.HasPrefix(plain, "@@") {
			// extract starting line from @@ -old +new @@
			var newStart int
			fmt.Sscanf(plain, "@@ -%*d,%*d +%d", &newStart)
			if newStart == 0 {
				fmt.Sscanf(plain, "@@ -%*d +%d", &newStart)
			}
			if newStart > 0 {
				currentLine = newStart - 1 // will be incremented below
			}
		}
		// context and added lines advance the file line counter
		if !strings.HasPrefix(plain, "-") && !strings.HasPrefix(plain, "\\") &&
			!strings.HasPrefix(plain, "diff ") && !strings.HasPrefix(plain, "index ") &&
			!strings.HasPrefix(plain, "---") && !strings.HasPrefix(plain, "+++") &&
			!strings.HasPrefix(plain, "@@") && plain != "" {
			currentLine++
		}

		out = append(out, line)

		// inject thread after this line if one is anchored here — delete after
		// injection so it only fires once even if currentLine stays the same
		if t, ok := lineThreads[currentLine]; ok {
			out = append(out, renderThreadBlock(t, width)...)
			delete(lineThreads, currentLine)
		}
	}
	return out
}

func renderThreadBlock(t *gh.NoteThread, width int) []string {
	styleBox := lipgloss.NewStyle().
		Foreground(lipgloss.Color("252")).
		Background(lipgloss.Color("236"))
	styleAuthorLocal := lipgloss.NewStyle().
		Foreground(lipgloss.Color("99")).Bold(true)
	styleAuthorGH := lipgloss.NewStyle().
		Foreground(lipgloss.Color("33")).Bold(true)
	styleDim := lipgloss.NewStyle().
		Foreground(lipgloss.Color("240"))
	styleBody := lipgloss.NewStyle().
		Foreground(lipgloss.Color("252"))
	styleSep := lipgloss.NewStyle().
		Foreground(lipgloss.Color("240"))

	boxW := width - 2
	if boxW < 10 {
		boxW = 10
	}

	var lines []string
	top := threadSentinel + styleSep.Render("  ┌" + strings.Repeat("─", boxW-2) + "┐")
	lines = append(lines, top)

	for i, msg := range t.Messages {
		authorStyle := styleAuthorGH
		if !msg.FromGH {
			authorStyle = styleAuthorLocal
		}
		author := authorStyle.Render(msg.Author)
		age := styleDim.Render(formatAge(msg.CreatedAt))
		header := threadSentinel + styleBox.Render(fmt.Sprintf("  │ %s %s", author, age))
		lines = append(lines, header)

		for _, bodyLine := range strings.Split(msg.Body, "\n") {
			rendered := threadSentinel + styleBox.Render("  │ "+styleBody.Render(truncate(bodyLine, boxW-4)))
			lines = append(lines, rendered)
		}

		if i < len(t.Messages)-1 {
			sep := threadSentinel + styleSep.Render("  ├" + strings.Repeat("─", boxW-2) + "┤")
			lines = append(lines, sep)
		}
	}

	bottom := threadSentinel + styleSep.Render("  └" + strings.Repeat("─", boxW-2) + "┘")
	lines = append(lines, bottom)
	return lines
}

func formatAge(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
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
	if m.mode == ModeNote || m.mode == ModeReply {
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

// ── overlays ──────────────────────────────────────────────────────────────────

var styleOverlay = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(lipgloss.Color("99")).
	Padding(1, 2)

func (m Model) renderNoteEditor() string {
	title := fmt.Sprintf(" note: %s ", m.notePath)
	if m.noteLine > 0 {
		title = fmt.Sprintf(" note: %s:%d ", m.notePath, m.noteLine)
	}
	if m.mode == ModeReply {
		title = fmt.Sprintf(" reply: %s:%d ", m.notePath, m.noteLine)
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
	return centreBox(box, m.width, m.height)
}

func (m Model) renderNotesList() string {
	header := m.renderHeader()
	hintStr := "n toggle · R reply · esc back"
	if m.pr != nil {
		hintStr += " · P publish"
	}
	footer := styleFooter.Width(m.width).Render(hintStr)
	usable := m.height - lipgloss.Height(header) - lipgloss.Height(footer)

	styleTitle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99"))
	styleAuthor := lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	var sb strings.Builder
	if m.threadCount() == 0 {
		sb.WriteString(styleDim.Render("  no threads yet"))
	} else {
		for key, t := range m.threads {
			if len(t.Messages) == 0 {
				continue
			}
			loc := t.Path
			if t.Line > 0 {
				loc = fmt.Sprintf("%s:%d", t.Path, t.Line)
			}
			sb.WriteString(styleTitle.Render(fmt.Sprintf("  ● %s", loc)))
			if !t.Visible {
				sb.WriteString(styleDim.Render(" (hidden)"))
			}
			sb.WriteByte('\n')
			for _, msg := range t.Messages {
				author := styleAuthor.Render(msg.Author)
				sb.WriteString(fmt.Sprintf("    %s: %s\n", author, truncate(msg.Body, m.width-12)))
			}
			sb.WriteByte('\n')
			_ = key
		}
	}
	content := lipgloss.NewStyle().Width(m.width).Height(usable).Render(sb.String())
	return lipgloss.JoinVertical(lipgloss.Left, header, content, footer)
}

func (m Model) renderViewedList() string {
	header := m.renderHeader()
	footer := styleFooter.Width(m.width).Render("enter unview · esc back")
	usable := m.height - lipgloss.Height(header) - lipgloss.Height(footer)
	paths := m.viewedPaths()

	var sb strings.Builder
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	if len(paths) == 0 {
		sb.WriteString(styleDim.Render("  no viewed files"))
	} else {
		for i, p := range paths {
			prefix := "  "
			line := styleDim.Render(prefix + "● " + p)
			if i == m.cursor {
				plain := prefix + "● " + p
				padded := plain + strings.Repeat(" ", max(0, m.width-len([]rune(plain))))
				if len([]rune(padded)) > m.width {
					padded = string([]rune(padded)[:m.width])
				}
				line = styleSelected.Render(padded) + "\033[0m"
			}
			sb.WriteString(line + "\n")
		}
	}
	content := lipgloss.NewStyle().Width(m.width).Height(usable).Render(sb.String())
	return lipgloss.JoinVertical(lipgloss.Left, header, content, footer)
}

// ── help popup ────────────────────────────────────────────────────────────────

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
  z          zoom preview full-screen (toggle)
  click      click file in list to select
             click line in preview to select it for annotation

 ── View ─────────────────────────────────────────────────
  t          toggle diff / whole-file view

 ── Search ───────────────────────────────────────────────
  /          grep changed files  (enter to search, esc cancel)
  f          back to file list
  p          peek: fuzzy-find any repo file (type to filter)
  r          refresh

 ── Review ───────────────────────────────────────────────
  v          mark file viewed — hides it until its diff changes
  V          show viewed files (enter to unview)
  n          new thread on current file / toggle thread visibility
  R          reply to thread at selected line
  N          view all threads
{{PR_PUBLISH}}
 ── Invocation ───────────────────────────────────────────
  wake                       local changes vs HEAD
  wake --since main          everything on this branch
  wake --pr 42               review GitHub PR #42
  wake --pr <url>            review by full GitHub URL

 ── Config  ~/.config/wake/config.toml or .wake.toml ─────
  editor, preview, preview_width, sort, layout, exclude

 press any key to close
`

func (m Model) renderHelp() string {
	text := strings.TrimPrefix(helpText, "\n")
	if m.pr != nil {
		text = strings.ReplaceAll(text, "{{PR_PUBLISH}}", "  P          publish threads as GitHub PR review\n")
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
	box := styleOverlay.Render(strings.Join(rendered, "\n"))
	return centreBox(box, m.width, m.height)
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
	styleThread   = lipgloss.NewStyle().Foreground(lipgloss.Color("99")).Bold(true)
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
		return lipgloss.NewStyle().Width(width).Height(height).
			Foreground(lipgloss.Color("240")).Render(msg)
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

	var sb strings.Builder
	for ri := startR; ri < endR; ri++ {
		r := rows[ri]
		var line string
		if r.kind == 0 {
			line = styleDirHeader.Render("  "+r.dir+"/") + "\033[0m"
			line = padRight(line, width)
		} else {
			it := items[r.itemIndex]
			st := statusStyle(it.Status).Render(it.Status)

			// thread marker
			threadMark := ""
			filePath := it.Path
			for _, t := range m.threads {
				if t.Path == filePath && len(t.Messages) > 0 {
					threadMark = styleThread.Render("●") + " "
					break
				}
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

			line = fmt.Sprintf("%s%s  %s%s", indent, st, threadMark, label)
			line = truncate(line, width)
			line = padRight(line, width)

			if r.itemIndex == cursor {
				plain := stripANSI(line)
				padded := plain + strings.Repeat(" ", max(0, width-len([]rune(plain))))
				if len([]rune(padded)) > width {
					padded = string([]rune(padded)[:width])
				}
				line = styleSelected.Render(padded) + "\033[0m"
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
		Bold(true).
		MaxWidth(width)

	styleThreadLine := lipgloss.NewStyle().
		Background(lipgloss.Color("236")).
		Foreground(lipgloss.Color("252"))

	var sb strings.Builder
	for i, line := range visible {
		absoluteI := start + i
		isThread := strings.HasPrefix(line, threadSentinel)
		displayLine := strings.TrimPrefix(line, threadSentinel)

		if isThread {
			plain := stripANSI(displayLine)
			padded := plain + strings.Repeat(" ", max(0, width-len([]rune(plain))))
			if len([]rune(padded)) > width {
				padded = string([]rune(padded)[:width])
			}
			sb.WriteString(styleThreadLine.Render(padded) + "\033[0m")
			sb.WriteByte('\n')
			continue
		}

		if m.previewClickedRow >= 0 && absoluteI == m.previewClickedRow {
			plain := stripANSI(displayLine)
			padded := plain + strings.Repeat(" ", max(0, width-len([]rune(plain))))
			if len([]rune(padded)) > width {
				padded = string([]rune(padded)[:width])
			}
			sb.WriteString(styleClickedLine.Render(padded) + "\033[0m")
			sb.WriteByte('\n')
			continue
		}

		if len([]rune(stripANSI(displayLine))) > width {
			displayLine = truncateANSI(displayLine, width)
		}
		sb.WriteString(displayLine + "\033[0m")
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

func appendComments(preview, path string, threads map[string]*gh.NoteThread) string {
	// No longer used — threads are injected inline. Kept for compatibility.
	return preview
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
		color := lipgloss.Color("33")
		if !m.prRepoMatch {
			color = lipgloss.Color("214") // amber = different repo warning
		}
		prLabel = lipgloss.NewStyle().Foreground(color).
			Render(" [" + truncate(title, 50) + "]")
		if !m.prRepoMatch {
			prLabel += lipgloss.NewStyle().Foreground(lipgloss.Color("214")).
				Render(" ⚠ external repo")
		}
	}

	threadBadge := ""
	if m.threadCount() > 0 {
		threadBadge = lipgloss.NewStyle().Foreground(lipgloss.Color("99")).Bold(true).
			Render(fmt.Sprintf(" ●%d", m.threadCount()))
	}

	viewedBadge := ""
	if len(m.viewed) > 0 {
		viewedBadge = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).
			Render(fmt.Sprintf(" ✓%d", len(m.viewed)))
	}

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
	right := fmt.Sprintf("%s%s%s  %s", count, threadBadge, viewedBadge, brand)
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
			hints = fmt.Sprintf("j/k scroll · %s · R reply · z unzoom", noteHint)
		} else if m.focus == focusPreview {
			noteHint := "n note"
			if m.previewClickedLine > 0 {
				noteHint = fmt.Sprintf("n note@line%d", m.previewClickedLine)
			} else if m.previewClickedRow >= 0 {
				noteHint = fmt.Sprintf("n note@row%d", m.previewClickedRow+1)
			}
			hints = fmt.Sprintf("j/k scroll · %s · R reply · h back to list", noteHint)
		} else {
			base := fmt.Sprintf("enter open · v viewed · V viewed-list · t %s · / grep · p peek · l preview · z zoom · n note · R reply · N threads · r refresh · H help", toggle)
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
		hints = "n toggle · R reply · esc back"
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
		return "threads>"
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

func buildTreeRows(items []git.Item) []struct {
	kind      int
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

func centreBox(box string, w, h int) string {
	bw := lipgloss.Width(box)
	bh := lipgloss.Height(box)
	padL := (w - bw) / 2
	padT := (h - bh) / 2
	if padL < 0 {
		padL = 0
	}
	if padT < 0 {
		padT = 0
	}
	left := strings.Repeat(" ", padL)
	var sb strings.Builder
	sb.WriteString(strings.Repeat("\n", padT))
	for _, line := range strings.Split(box, "\n") {
		sb.WriteString(left + line + "\n")
	}
	return sb.String()
}

func extractLineNumber(line string) int {
	s := stripANSI(line)
	s = strings.TrimLeft(s, " ")
	if strings.HasPrefix(s, "@@") {
		var new_ int
		fmt.Sscanf(s, "@@ -%*d,%*d +%d", &new_)
		if new_ == 0 {
			fmt.Sscanf(s, "@@ -%*d +%d", &new_)
		}
		return new_
	}
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
