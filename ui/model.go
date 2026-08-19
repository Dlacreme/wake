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
	ModeList      Mode = iota // changed-file list
	ModeGrep                  // grep results for changed files
	ModePeek                  // whole-repo file list
	ModePeekGrep              // grep results for whole repo
	ModeNote                  // note editor overlay
	ModeNotesList             // view all pending notes
)

// ── messages ──────────────────────────────────────────────────────────────────

type itemsLoadedMsg struct{ items []git.Item }
type previewReadyMsg struct{ content string }
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
	// state
	mode   Mode
	items  []git.Item // displayed rows
	cursor int
	query  textinput.Model

	// viewed: path → diffHash at mark time (session-only)
	viewed map[string]string

	// preview
	preview string
	layout  previewLayout
	full    bool // diff=false / full-file=true

	// peek sub-state (saved/restored on ctrl-p / esc)
	peekItems       []git.Item
	peekCursor      int
	peekQuery       textinput.Model
	savedListCursor int
	savedListItems  []git.Item
	savedMode       Mode

	// dimensions
	width  int
	height int

	// config + git root
	cfg   config.Config
	root  string
	since string // --since value (empty = HEAD)

	// PR mode
	pr           *gh.PR            // nil when not in PR mode
	prComments   []gh.ReviewComment // existing comments from GitHub
	prFullDiff   string             // full unified diff from gh pr diff
	prLoading    bool

	// notes (session-only; publishable if pr != nil)
	notes     map[string]gh.Note // key: path (one note per file for now)
	noteTA    textarea.Model     // the active textarea when ModeNote
	notePath  string             // file being noted
	noteLine  int                // line (0 = file-level)

	// status line message (transient)
	statusMsg string
}

// New creates the initial model.
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
		mode:      ModeList,
		viewed:    make(map[string]string),
		notes:     make(map[string]gh.Note),
		query:     qi,
		peekQuery: pi,
		noteTA:    ta,
		cfg:       cfg,
		root:      root,
		since:     since,
		full:      cfg.Preview == "full",
		pr:        pr,
		prLoading: pr != nil,
	}
}

// ── Bubble Tea interface ───────────────────────────────────────────────────────

func (m Model) Init() tea.Cmd {
	if m.pr != nil {
		// PR mode: load PR metadata first; items come from the PR file list
		return loadPR(m.pr)
	}
	return loadItems(m.root, m.since, m.cfg.Exclude, m.cfg.Sort)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.noteTA.SetWidth(m.width - 4)
		return m, m.refreshPreviewCmd()

	case itemsLoadedMsg:
		switch m.mode {
		case ModeList, ModeGrep:
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
		// Build item list from PR file list
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
		return m, nil

	case editorDoneMsg:
		return m, m.refreshPreviewCmd()

	case errMsg:
		m.statusMsg = "error: " + msg.err.Error()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// clear status message on any keypress
	m.statusMsg = ""

	// note editor captures all keys except save/cancel
	if m.mode == ModeNote {
		return m.handleNoteKey(msg)
	}

	isModePeek := m.mode == ModePeek || m.mode == ModePeekGrep

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

	case key.Matches(msg, keys.Viewed):
		if !isModePeek && m.mode != ModeNotesList {
			return m.markViewed()
		}

	case key.Matches(msg, keys.Toggle):
		if !isModePeek {
			m.full = !m.full
			return m, m.refreshPreviewCmd()
		}

	case key.Matches(msg, keys.Refresh):
		m.statusMsg = ""
		if isModePeek {
			return m, loadPeekItems(m.root)
		}
		return m, loadItems(m.root, m.since, m.cfg.Exclude, m.cfg.Sort)

	case key.Matches(msg, keys.Layout):
		m.layout = m.layout.next()
		return m, nil

	case key.Matches(msg, keys.Grep):
		return m.handleGrep()

	case key.Matches(msg, keys.FileList):
		return m.handleFileList()

	case key.Matches(msg, keys.Peek):
		if !isModePeek && m.mode != ModeNotesList {
			return m.handlePeekOpen()
		}

	case key.Matches(msg, keys.Note):
		if !isModePeek {
			return m.handleNoteOpen()
		}

	case key.Matches(msg, keys.NotesList):
		if !isModePeek {
			return m.handleNotesList()
		}

	case key.Matches(msg, keys.Publish):
		if m.pr != nil && len(m.notes) > 0 {
			return m, publishNotes(m.pr, m.notesSlice(), m.prFullDiff)
		} else if m.pr != nil {
			m.statusMsg = "no notes to publish"
		} else {
			m.statusMsg = "not in PR mode (use --pr)"
		}

	default:
		var cmd tea.Cmd
		if isModePeek {
			m.peekQuery, cmd = m.peekQuery.Update(msg)
			if m.mode == ModePeek {
				m.filterPeekByQuery()
				m.clampPeekCursor()
				return m, tea.Batch(cmd, m.refreshPreviewCmd())
			}
		} else {
			m.query, cmd = m.query.Update(msg)
			if m.mode == ModeList {
				m.filterListByQuery()
				m.clampCursor()
				return m, tea.Batch(cmd, m.refreshPreviewCmd())
			}
		}
		return m, cmd
	}

	return m, nil
}

// ── note editor ───────────────────────────────────────────────────────────────

func (m Model) handleNoteOpen() (Model, tea.Cmd) {
	item := m.selectedItem()
	if item == nil {
		return m, nil
	}
	m.notePath = item.Path
	m.noteLine = item.Line

	// pre-fill with existing note if any
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
			// empty note = delete
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

// ── navigation ────────────────────────────────────────────────────────────────

func (m Model) moveCursor(delta int) (Model, tea.Cmd) {
	isPeek := m.mode == ModePeek || m.mode == ModePeekGrep
	if isPeek {
		m.peekCursor = clamp(m.peekCursor+delta, 0, len(m.peekItems)-1)
	} else {
		m.cursor = clamp(m.cursor+delta, 0, len(m.items)-1)
	}
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
		return m, loadItems(m.root, m.since, m.cfg.Exclude, m.cfg.Sort)
	case ModeNotesList:
		m.mode = ModeList
		return m, m.refreshPreviewCmd()
	}
	return m, nil
}

func (m Model) handleEnter() (Model, tea.Cmd) {
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

func (m Model) handleGrep() (Model, tea.Cmd) {
	q := m.activeQuery()
	if m.mode == ModePeek || m.mode == ModePeekGrep {
		m.mode = ModePeekGrep
		m.peekItems = nil
		return m, grepAll(m.root, q)
	}
	m.mode = ModeGrep
	m.items = nil
	return m, grepChanged(m.root, m.since, q, m.cfg.Exclude, m.cfg.Sort)
}

func (m Model) handleFileList() (Model, tea.Cmd) {
	if m.mode == ModePeek || m.mode == ModePeekGrep {
		m.mode = ModePeek
		m.peekQuery.SetValue("")
		return m, loadPeekItems(m.root)
	}
	m.mode = ModeList
	m.query.SetValue("")
	return m, loadItems(m.root, m.since, m.cfg.Exclude, m.cfg.Sort)
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

// prFilesToItems builds the item list from a PR file list + diff.
// Status is inferred from the diff headers (new file / deleted file / modified).
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
		fmtStr := cfg.EditorLineFmt
		fmtStr = strings.ReplaceAll(fmtStr, "{file}", full)
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

	return tea.ExecProcess(c, func(err error) tea.Msg {
		return editorDoneMsg{}
	})
}

func (m Model) refreshPreviewCmd() tea.Cmd {
	item := m.selectedItem()
	if item == nil {
		return func() tea.Msg { return previewReadyMsg{""} }
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
			// PR mode: always render from the PR diff
			fileDiff := gh.FileDiff(prFullDiff, it.Path)
			if fileDiff != "" {
				content = renderDiffText(fileDiff, w)
			}
		} else {
			content = renderPreview(root, since, it, full, w)
		}
		content = appendComments(content, it.Path, comments)
		content = appendNote(content, it.Path, notes)
		return previewReadyMsg{content}
	}
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m Model) View() string {
	if m.width == 0 {
		return ""
	}

	// note editor overlay
	if m.mode == ModeNote {
		return m.renderNoteEditor()
	}

	// notes list overlay
	if m.mode == ModeNotesList {
		return m.renderNotesList()
	}

	header := m.renderHeader()
	footer := m.renderFooter()
	usable := m.height - lipgloss.Height(header) - lipgloss.Height(footer)
	if usable < 1 {
		usable = 1
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

// ── note editor overlay ───────────────────────────────────────────────────────

var styleOverlay = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(lipgloss.Color("99")).
	Padding(1, 2)

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
	// centre the box
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
	top := strings.Repeat("\n", padT)
	left := strings.Repeat(" ", padL)
	var sb strings.Builder
	sb.WriteString(top)
	for _, line := range strings.Split(box, "\n") {
		sb.WriteString(left)
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// ── notes list overlay ────────────────────────────────────────────────────────

func (m Model) renderNotesList() string {
	header := m.renderHeader()
	footer := styleFooter.Width(m.width).Render("n edit · esc back" +
		func() string {
			if m.pr != nil {
				return " · ctrl-s publish"
			}
			return ""
		}())

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

	content := lipgloss.NewStyle().
		Width(m.width).Height(usable).
		Render(sb.String())

	return lipgloss.JoinVertical(lipgloss.Left, header, content, footer)
}

// ── list pane ─────────────────────────────────────────────────────────────────

var (
	styleSelected = lipgloss.NewStyle().
			Background(lipgloss.Color("237")).
			Bold(true)

	styleStatusM = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	styleStatusA = lipgloss.NewStyle().Foreground(lipgloss.Color("76")).Bold(true)
	styleStatusD = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	styleStatusR = lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Bold(true)
	styleStatusG = lipgloss.NewStyle().Foreground(lipgloss.Color("165")).Bold(true)
	styleStatusP = lipgloss.NewStyle().Foreground(lipgloss.Color("44")).Bold(true)
	styleNote    = lipgloss.NewStyle().Foreground(lipgloss.Color("99")).Bold(true)
)

func (m Model) renderSideBySide(header, footer string, usable int) string {
	listW := m.listWidth()
	prevW := m.width - listW - 1

	list := m.renderList(listW, usable)
	prev := m.renderPreviewPane(prevW, usable)

	divStyle := lipgloss.NewStyle().
		Width(1).
		Height(usable).
		Foreground(lipgloss.Color("240"))

	row := lipgloss.JoinHorizontal(lipgloss.Top,
		list,
		divStyle.Render("│"),
		prev,
	)
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

	start, end := scrollWindow(cursor, len(items), height)

	var sb strings.Builder
	for i := start; i < end; i++ {
		it := items[i]
		stStyle := statusStyle(it.Status)
		st := stStyle.Render(it.Status)

		// note marker
		noteMark := ""
		if _, hasNote := m.notes[it.Path]; hasNote {
			noteMark = styleNote.Render("●") + " "
		}

		var label string
		if it.Status == git.StatusGrep {
			label = fmt.Sprintf("%s:%d  %s", it.Path, it.Line, truncate(it.Text, width-20))
		} else {
			label = it.Path
		}

		line := fmt.Sprintf("  %s  %s%s", st, noteMark, label)
		line = truncate(line, width)
		line = padRight(line, width)

		if i == cursor {
			line = styleSelected.Width(width).Render(line)
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
	content := m.preview
	lines := strings.Split(content, "\n")

	if len(lines) > height {
		lines = lines[:height]
	}

	var sb strings.Builder
	for _, line := range lines {
		visible := stripANSI(line)
		if len(visible) > width {
			line = truncateANSI(line, width)
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	rendered := sb.String()
	lineCount := strings.Count(rendered, "\n")
	for lineCount < height {
		rendered += "\n"
		lineCount++
	}
	return rendered
}

// appendComments appends existing PR review comments for a file to the preview.
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
	sb.WriteString(lipgloss.NewStyle().
		Foreground(lipgloss.Color("240")).
		Render(strings.Repeat("─", 40)))
	sb.WriteString("\n")
	for _, c := range relevant {
		header := lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.Color("33")).
			Render(fmt.Sprintf("  @%s", c.Author))
		if c.Line > 0 {
			header += lipgloss.NewStyle().
				Foreground(lipgloss.Color("240")).
				Render(fmt.Sprintf(" line %d", c.Line))
		}
		sb.WriteString(header + "\n")
		for _, line := range strings.Split(c.Body, "\n") {
			sb.WriteString("  " + line + "\n")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// appendNote appends the pending local note for a file to the preview.
func appendNote(preview, path string, notes map[string]gh.Note) string {
	note, ok := notes[path]
	if !ok {
		return preview
	}
	var sb strings.Builder
	sb.WriteString(preview)
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().
		Foreground(lipgloss.Color("99")).
		Render(strings.Repeat("─", 40)))
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Bold(true).
		Foreground(lipgloss.Color("99")).Render("  ● your note") + "\n")
	for _, line := range strings.Split(note.Body, "\n") {
		sb.WriteString("  " + line + "\n")
	}
	return sb.String()
}

// ── header / footer ───────────────────────────────────────────────────────────

var styleHeader = lipgloss.NewStyle().
	Background(lipgloss.Color("235")).
	Foreground(lipgloss.Color("252")).
	Bold(false).
	Padding(0, 1)

var styleFooter = lipgloss.NewStyle().
	Foreground(lipgloss.Color("240")).
	Padding(0, 1)

var styleBrand = lipgloss.NewStyle().
	Foreground(lipgloss.Color("99")).
	Bold(true)

func (m Model) renderHeader() string {
	prompt := m.promptLabel()
	query := m.activeQuery()

	// PR title in header when in PR mode
	prLabel := ""
	if m.pr != nil {
		title := m.pr.Title
		if title == "" {
			title = fmt.Sprintf("PR #%d", m.pr.Number)
		} else {
			title = fmt.Sprintf("PR #%d: %s", m.pr.Number, title)
		}
		prLabel = lipgloss.NewStyle().
			Foreground(lipgloss.Color("33")).
			Render(" [" + truncate(title, 50) + "]")
	}

	// note count badge
	noteBadge := ""
	if len(m.notes) > 0 {
		noteBadge = lipgloss.NewStyle().
			Foreground(lipgloss.Color("99")).Bold(true).
			Render(fmt.Sprintf(" ●%d", len(m.notes)))
	}

	count := fmt.Sprintf("%d/%d", m.activeCursor()+1, len(m.activeItems()))
	if len(m.activeItems()) == 0 {
		count = "0/0"
	}
	brand := styleBrand.Render("wake")
	left := fmt.Sprintf("%s  %s%s", prompt, query, prLabel)
	right := fmt.Sprintf("%s%s  %s", count, noteBadge, brand)
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
		base := fmt.Sprintf("enter open · v viewed · ctrl-d %s · ctrl-g grep · ctrl-p peek · n note · N notes · ctrl-r refresh · ctrl-/ layout", toggle)
		if m.pr != nil {
			base += " · ctrl-s publish"
		}
		hints = base
	case ModeGrep:
		hints = "enter open · v viewed · n note · ctrl-f file list · ctrl-r refresh"
	case ModePeek:
		hints = "enter open · ctrl-g grep repo · ctrl-f file list · esc back"
	case ModePeekGrep:
		hints = "enter open · ctrl-f file list · esc back"
	case ModeNotesList:
		hints = "esc back"
		if m.pr != nil {
			hints += " · ctrl-s publish"
		}
	}

	if m.statusMsg != "" {
		msg := lipgloss.NewStyle().
			Foreground(lipgloss.Color("214")).Bold(true).
			Render(m.statusMsg + "  ")
		// status on top, hints below
		return lipgloss.JoinVertical(lipgloss.Left,
			styleFooter.Width(m.width).Render(msg),
			styleFooter.Width(m.width).Render(hints),
		)
	}

	return styleFooter.Width(m.width).Render(hints)
}

func (m Model) promptLabel() string {
	switch m.mode {
	case ModeGrep:
		return "grep>"
	case ModePeek:
		return "peek>"
	case ModePeekGrep:
		return "repo-grep>"
	case ModeNotesList:
		return "notes>"
	default:
		return "changed>"
	}
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

