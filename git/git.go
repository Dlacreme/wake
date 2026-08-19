package git

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Status values
const (
	StatusModified = "M"
	StatusAdded    = "A"
	StatusDeleted  = "D"
	StatusRenamed  = "R"
	StatusGrep     = "G"
	StatusPeek     = "P"
)

// Item is a single row in the changed-file list or grep results.
type Item struct {
	Status string // M A D R G P
	Path   string
	Line   int    // >0 for grep/hunk hits
	Text   string // grep hit display text
}

// Root returns the git repo root for the cwd, or an error.
func Root() (string, error) {
	out, err := run("", "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not inside a git repository")
	}
	return strings.TrimSpace(out), nil
}

// ValidateRef returns an error if ref is not a valid git ref.
func ValidateRef(root, ref string) error {
	_, err := run(root, "git", "rev-parse", "--verify", ref)
	if err != nil {
		return fmt.Errorf("bad ref %q: %w", ref, err)
	}
	return nil
}

// ChangedItems returns the normalised changed-file list, recency-sorted,
// with exclude globs applied. since may be empty (vs HEAD).
func ChangedItems(root, since string, exclude []string) ([]Item, error) {
	raw, err := rawLines(root, since)
	if err != nil {
		return nil, err
	}
	items := normalise(raw, since)
	items = applyExclude(items, exclude)
	items = sortByMtime(root, items)
	return items, nil
}

// GrepItems runs ripgrep (or grep) restricted to the changed-file set.
// Returns G-status items.
func GrepItems(root, since, query string, exclude []string) ([]Item, error) {
	if query == "" {
		return nil, nil
	}
	all, err := ChangedItems(root, since, exclude)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, it := range all {
		if it.Status != StatusDeleted {
			paths = append(paths, it.Path)
		}
	}
	if len(paths) == 0 {
		return nil, nil
	}
	return runGrep(root, query, paths)
}

// GrepAll runs ripgrep (or grep) over the entire repo.
func GrepAll(root, query string) ([]Item, error) {
	if query == "" {
		return nil, nil
	}
	return runGrep(root, query, []string{"."})
}

// RepoFiles returns every tracked + untracked file (for peek mode).
func RepoFiles(root string) ([]Item, error) {
	out, err := run(root, "git", "-c", "core.quotepath=false",
		"ls-files", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	var items []Item
	for _, line := range splitLines(out) {
		if line == "" {
			continue
		}
		items = append(items, Item{Status: StatusPeek, Path: line})
	}
	return items, nil
}

// DiffText returns the coloured diff for a single file.
// Callers (preview) can shell to delta on top of this.
func DiffText(root, since, path string, colour bool) string {
	ref := since
	if ref == "" {
		ref = "HEAD"
	}
	flag := "--no-color"
	if colour {
		flag = "--color=always"
	}
	out, _ := run(root, "git", "diff", flag, ref, "--", path)
	return out
}

// FileText returns the content of a file, raw (preview shells to bat).
func FileText(root, path string) string {
	full := filepath.Join(root, path)
	b, err := os.ReadFile(full)
	if err != nil {
		return fmt.Sprintf("cannot read %s: %v", path, err)
	}
	return string(b)
}

// HunkLine returns the first changed line number in a file's diff.
func HunkLine(root, since, path string) int {
	ref := since
	if ref == "" {
		ref = "HEAD"
	}
	out, _ := run(root, "git", "diff", "-U0", ref, "--", path)
	for _, line := range splitLines(out) {
		// @@ -old +new[,count] @@
		if !strings.HasPrefix(line, "@@") {
			continue
		}
		// extract the +N part
		parts := strings.Fields(line)
		for _, p := range parts {
			if strings.HasPrefix(p, "+") {
				n := strings.TrimPrefix(p, "+")
				n = strings.SplitN(n, ",", 2)[0]
				if ln, err := strconv.Atoi(n); err == nil && ln > 0 {
					return ln
				}
			}
		}
	}
	return 1
}

// DiffHash returns a hex string of sha256(diff output) for a file.
// Used to detect "this file changed since we marked it viewed."
func DiffHash(root, since, path string) string {
	text := DiffText(root, since, path, false)
	sum := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%x", sum[:8])
}

// ── internal ──────────────────────────────────────────────────────────────────

func rawLines(root, since string) ([]string, error) {
	var out string
	var err error
	if since != "" {
		out, err = run(root, "git", "-c", "core.quotepath=false",
			"diff", "--name-status", since, "--")
		if err != nil {
			return nil, err
		}
		// untracked files
		untracked, _ := run(root, "git", "-c", "core.quotepath=false",
			"ls-files", "--others", "--exclude-standard")
		for _, f := range splitLines(untracked) {
			if f != "" {
				out += "\nA\t" + f
			}
		}
	} else {
		out, err = run(root, "git", "-c", "core.quotepath=false",
			"status", "--porcelain=v1", "-uall")
		if err != nil {
			return nil, err
		}
	}
	return splitLines(out), nil
}

func normalise(lines []string, since string) []Item {
	var items []Item
	seen := map[string]bool{}
	for _, line := range lines {
		if line == "" {
			continue
		}
		var st, path string
		if since != "" {
			// "STATUS\tpath" or "R100\told\tnew"
			parts := strings.SplitN(line, "\t", 3)
			if len(parts) < 2 {
				continue
			}
			st = string([]rune(parts[0])[0]) // first char only (R100 → R)
			if st == "R" || st == "C" {
				if len(parts) == 3 {
					path = parts[2]
				} else {
					path = parts[1]
				}
				st = "R"
			} else {
				path = parts[1]
			}
		} else {
			// porcelain v1: "XY path" or "XY old -> new"
			if len(line) < 4 {
				continue
			}
			xy := line[:2]
			rest := line[3:]
			switch {
			case xy == "??":
				st = StatusAdded
			case strings.ContainsAny(xy, "R"):
				st = StatusRenamed
				if idx := strings.LastIndex(rest, " -> "); idx >= 0 {
					rest = rest[idx+4:]
				}
			case strings.ContainsAny(xy, "D"):
				st = StatusDeleted
			case strings.ContainsAny(xy, "A"):
				st = StatusAdded
			default:
				st = StatusModified
			}
			path = rest
		}
		// strip git quoting
		path = strings.Trim(path, "\"")
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		items = append(items, Item{Status: st, Path: path})
	}
	return items
}

func applyExclude(items []Item, globs []string) []Item {
	if len(globs) == 0 {
		return items
	}
	var out []Item
	for _, it := range items {
		excluded := false
		for _, g := range globs {
			if matched, _ := filepath.Match(g, it.Path); matched {
				excluded = true
				break
			}
			// also try matching just the base name
			if matched, _ := filepath.Match(g, filepath.Base(it.Path)); matched {
				excluded = true
				break
			}
		}
		if !excluded {
			out = append(out, it)
		}
	}
	return out
}

type itemWithMtime struct {
	item  Item
	mtime float64
}

func sortByMtime(root string, items []Item) []Item {
	withM := make([]itemWithMtime, len(items))
	for i, it := range items {
		withM[i] = itemWithMtime{item: it, mtime: mtime(filepath.Join(root, it.Path))}
	}
	sort.SliceStable(withM, func(i, j int) bool {
		return withM[i].mtime > withM[j].mtime
	})
	out := make([]Item, len(items))
	for i, w := range withM {
		out[i] = w.item
	}
	return out
}


func runGrep(root, query string, paths []string) ([]Item, error) {
	var args []string
	var cmd string

	if haveBin("rg") {
		cmd = "rg"
		args = append(args, "-H", "--line-number", "--no-heading",
			"--color=never", "--smart-case", "-e", query, "--")
		args = append(args, paths...)
	} else {
		cmd = "grep"
		args = append(args, "-HRnI", "-e", query, "--")
		args = append(args, paths...)
	}

	c := exec.Command(cmd, args...)
	c.Dir = root
	c.Stdin = openDevNull()
	out, _ := c.Output() // non-zero exit = no matches, not an error

	var items []Item
	for _, line := range splitLines(string(out)) {
		if line == "" {
			continue
		}
		// strip leading "./" that rg sometimes emits
		line = strings.TrimPrefix(line, "./")
		// format: path:line:text
		p1 := strings.Index(line, ":")
		if p1 < 0 {
			continue
		}
		rest := line[p1+1:]
		p2 := strings.Index(rest, ":")
		if p2 < 0 {
			continue
		}
		path := line[:p1]
		lineNum, _ := strconv.Atoi(rest[:p2])
		text := rest[p2+1:]
		items = append(items, Item{
			Status: StatusGrep,
			Path:   path,
			Line:   lineNum,
			Text:   text,
		})
	}
	return items, nil
}

func run(dir string, name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	if dir != "" {
		c.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	return stdout.String(), err
}

func splitLines(s string) []string {
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

func haveBin(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func openDevNull() *os.File {
	f, _ := os.Open(os.DevNull)
	return f
}
