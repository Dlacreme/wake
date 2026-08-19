package ui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/Dlacreme/wake/config"
	"github.com/Dlacreme/wake/git"
	"github.com/sahilm/fuzzy"
)

// ── modes ─────────────────────────────────────────────────────────────────────

type Mode int

const (
	ModeList     Mode = iota // changed-file list
	ModeGrep                 // grep results for changed files
	ModePeek                 // whole-repo file list
	ModePeekGrep             // grep results for whole repo
)

// ── messages ──────────────────────────────────────────────────────────────────

type itemsLoadedMsg struct{ items []git.Item }
type previewReadyMsg struct{ content string }
type editorDoneMsg struct{}
type errMsg struct{ err error }

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
	peekItems        []git.Item
	peekCursor       int
	peekQuery        textinput.Model
	savedListCursor  int
	savedListItems   []git.Item

	// dimensions
	width  int
	height int

	// config + git root
	cfg   config.Config
	root  string
	since string // --since value (empty = HEAD)

	// status line message (transient)
	statusMsg string
}

// New creates the initial model.
func New(root, since string, cfg config.Config) Model {
	qi := textinput.New()
	qi.Placeholder = ""
	qi.Prompt = ""

	pi := textinput.New()
	pi.Placeholder = ""
	pi.Prompt = ""

	return Model{
		mode:      ModeList,
		viewed:    make(map[string]string),
		query:     qi,
		peekQuery: pi,
		cfg:       cfg,
		root:      root,
		since:     since,
		full:      cfg.Preview == "full",
	}
}

// ── Bubble Tea interface ───────────────────────────────────────────────────────

func (m Model) Init() tea.Cmd {
	return loadItems(m.root, m.since, m.cfg.Exclude, m.cfg.Sort)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, m.refreshPreviewCmd()

	case itemsLoadedMsg:
		switch m.mode {
		case ModeList, ModeGrep:
			// filter out viewed (unless diff changed)
			m.items = m.filterViewed(msg.items)
			m.clampCursor()
		case ModePeek, ModePeekGrep:
			m.peekItems = msg.items
			m.clampPeekCursor()
		}
		return m, m.refreshPreviewCmd()

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
	// typing always feeds the query input
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
		if !isModePeek {
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
		if !isModePeek {
			return m.handlePeekOpen()
		}

	default:
		// feed key to the active query input
		var cmd tea.Cmd
		if isModePeek {
			m.peekQuery, cmd = m.peekQuery.Update(msg)
			// live fuzzy filter in peek file-list mode
			if m.mode == ModePeek {
				m.filterPeekByQuery()
				m.clampPeekCursor()
				return m, tea.Batch(cmd, m.refreshPreviewCmd())
			}
		} else {
			m.query, cmd = m.query.Update(msg)
			// live fuzzy filter in list mode
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
		// restore list state
		m.mode = ModeList
		m.cursor = m.savedListCursor
		m.items = m.savedListItems
		m.peekQuery.SetValue("")
		return m, m.refreshPreviewCmd()
	case ModeGrep:
		m.mode = ModeList
		m.query.SetValue("")
		return m, loadItems(m.root, m.since, m.cfg.Exclude, m.cfg.Sort)
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
	// remove from current list
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

// allChangedItems caches the unfiltered list across query changes.
// We reload from git on ctrl-r; during typing we fuzzy-filter this slice.
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

// filterViewed removes items whose diff hash matches the viewed hash.
func (m *Model) filterViewed(items []git.Item) []git.Item {
	if len(m.viewed) == 0 {
		return items
	}
	out := make([]git.Item, 0, len(items))
	for _, it := range items {
		if h, ok := m.viewed[it.Path]; ok {
			current := git.DiffHash(m.root, m.since, it.Path)
			if current == h {
				continue // same diff → still hidden
			}
			delete(m.viewed, it.Path) // changed → reappear
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
		// custom escape hatch
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
	return func() tea.Msg {
		content := renderPreview(root, since, it, full, w)
		return previewReadyMsg{content}
	}
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m Model) View() string {
	if m.width == 0 {
		return ""
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

func (m Model) renderSideBySide(header, footer string, usable int) string {
	listW := m.listWidth()
	prevW := m.width - listW - 1 // 1 for divider

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

// ── list pane ─────────────────────────────────────────────────────────────────

var (
	styleSelected = lipgloss.NewStyle().
			Background(lipgloss.Color("237")).
			Bold(true)

	styleStatusM = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true) // yellow
	styleStatusA = lipgloss.NewStyle().Foreground(lipgloss.Color("76")).Bold(true)  // green
	styleStatusD = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true) // red
	styleStatusR = lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Bold(true)  // blue
	styleStatusG = lipgloss.NewStyle().Foreground(lipgloss.Color("165")).Bold(true) // magenta
	styleStatusP = lipgloss.NewStyle().Foreground(lipgloss.Color("44")).Bold(true)  // cyan
)

func (m Model) renderList(width, height int) string {
	items := m.activeItems()
	cursor := m.activeCursor()

	if len(items) == 0 {
		empty := lipgloss.NewStyle().
			Width(width).Height(height).
			Foreground(lipgloss.Color("240")).
			Render("  nothing changed")
		return empty
	}

	// scroll window
	start, end := scrollWindow(cursor, len(items), height)

	var sb strings.Builder
	for i := start; i < end; i++ {
		it := items[i]
		stStyle := statusStyle(it.Status)
		st := stStyle.Render(it.Status)

		var label string
		if it.Status == git.StatusGrep {
			label = fmt.Sprintf("%s:%d  %s", it.Path, it.Line, truncate(it.Text, width-20))
		} else {
			label = it.Path
		}

		line := fmt.Sprintf("  %s  %s", st, label)
		line = truncate(line, width)
		line = padRight(line, width)

		if i == cursor {
			line = styleSelected.Width(width).Render(line)
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}

	// pad remaining lines
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

	// trim to height
	if len(lines) > height {
		lines = lines[:height]
	}

	var sb strings.Builder
	for _, line := range lines {
		// strip ANSI before measuring, then re-render truncated
		visible := stripANSI(line)
		if len(visible) > width {
			// truncate at visible width boundary
			line = truncateANSI(line, width)
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	// pad
	rendered := sb.String()
	lineCount := strings.Count(rendered, "\n")
	for lineCount < height {
		rendered += "\n"
		lineCount++
	}
	return rendered
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
	count := fmt.Sprintf("%d/%d", m.activeCursor()+1, len(m.activeItems()))
	if len(m.activeItems()) == 0 {
		count = "0/0"
	}
	brand := styleBrand.Render("wake")
	left := fmt.Sprintf("%s  %s", prompt, query)
	right := fmt.Sprintf("%s  %s", count, brand)
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}
	line := left + strings.Repeat(" ", gap) + right
	return styleHeader.Width(m.width).Render(line)
}

func (m Model) renderFooter() string {
	if m.statusMsg != "" {
		msg := m.statusMsg
		m.statusMsg = "" // clear after render (cosmetic; Update will clear on next action)
		return styleFooter.Width(m.width).Render(msg)
	}

	var hints string
	switch m.mode {
	case ModeList:
		toggle := "diff/full"
		if m.full {
			toggle = "full/diff"
		}
		hints = fmt.Sprintf("enter open · v viewed · ctrl-d %s · ctrl-g grep · ctrl-p peek · ctrl-r refresh · ctrl-/ layout", toggle)
	case ModeGrep:
		hints = "enter open · v viewed · ctrl-f file list · ctrl-r refresh"
	case ModePeek:
		hints = "enter open · ctrl-g grep repo · ctrl-f file list · esc back"
	case ModePeekGrep:
		hints = "enter open · ctrl-f file list · esc back"
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

// stripANSI removes ANSI escape sequences for length measurement.
func stripANSI(s string) string {
	var out strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\033' && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++ // skip 'm'
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

// truncateANSI truncates a string with ANSI codes to at most `max` visible chars.
func truncateANSI(s string, max int) string {
	var out strings.Builder
	visible := 0
	i := 0
	for i < len(s) && visible < max {
		if s[i] == '\033' && i+1 < len(s) && s[i+1] == '[' {
			// copy the escape sequence verbatim
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			j++ // include 'm'
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
