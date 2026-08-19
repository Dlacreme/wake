package ui

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/mathieu/wake/git"
)

// previewLayout cycles through preview pane positions.
type previewLayout int

const (
	layoutRight  previewLayout = iota // default: right 62%
	layoutBottom                      // bottom 40%
	layoutHidden                      // hidden
)

func (l previewLayout) next() previewLayout {
	return (l + 1) % 3
}

// renderPreview produces the string shown in the right/bottom pane.
// It shells out to bat/delta when available, same as the bash script.
func renderPreview(root, since string, item git.Item, full bool, width int) string {
	switch item.Status {
	case git.StatusGrep:
		return renderGrepHit(root, item)
	case git.StatusDeleted:
		return renderDeleted(root, since, item.Path)
	case git.StatusPeek, git.StatusAdded:
		return renderFile(root, item.Path)
	default:
		if full {
			return renderFile(root, item.Path)
		}
		return renderDiff(root, since, item.Path, width)
	}
}

func renderDiff(root, since, path string, width int) string {
	ref := since
	if ref == "" {
		ref = "HEAD"
	}
	if haveBin("delta") {
		raw := gitDiffRaw(root, ref, path)
		if raw == "" {
			return dim("(no diff)")
		}
		out, err := pipeToDelta(raw, width)
		if err == nil {
			return out
		}
		// delta failed — fall through to plain
	}
	out := gitDiffColour(root, ref, path)
	if out == "" {
		return dim("(no diff)")
	}
	return out
}

func renderFile(root, path string) string {
	if haveBin("bat") {
		c := exec.Command("bat", "--color=always", "--style=numbers",
			"--paging=never", "--", path)
		c.Dir = root
		out, err := c.Output()
		if err == nil {
			return string(out)
		}
	}
	// plain fallback
	text := git.FileText(root, path)
	var sb strings.Builder
	for i, line := range strings.Split(text, "\n") {
		fmt.Fprintf(&sb, "%4d  %s\n", i+1, line)
	}
	return sb.String()
}

func renderGrepHit(root string, item git.Item) string {
	if item.Line <= 0 {
		return item.Text
	}
	start := item.Line - 20
	if start < 1 {
		start = 1
	}
	end := item.Line + 40

	if haveBin("bat") {
		c := exec.Command("bat",
			"--color=always", "--style=numbers", "--paging=never",
			"--highlight-line", strconv.Itoa(item.Line),
			"--line-range", fmt.Sprintf("%d:%d", start, end),
			"--", item.Path)
		c.Dir = root
		out, err := c.Output()
		if err == nil {
			return string(out)
		}
	}
	// plain fallback: read lines start..end
	text := git.FileText(root, item.Path)
	lines := strings.Split(text, "\n")
	var sb strings.Builder
	for i, line := range lines {
		n := i + 1
		if n < start || n > end {
			continue
		}
		if n == item.Line {
			fmt.Fprintf(&sb, "%4d► %s\n", n, line)
		} else {
			fmt.Fprintf(&sb, "%4d  %s\n", n, line)
		}
	}
	return sb.String()
}

func renderDeleted(root, since, path string) string {
	header := "\033[1;31mdeleted\033[0m  " + path + "\n\n"
	return header + renderDiff(root, since, path, 100)
}

// ── shell helpers ─────────────────────────────────────────────────────────────

func gitDiffRaw(root, ref, path string) string {
	c := exec.Command("git", "diff", "--no-color", ref, "--", path)
	c.Dir = root
	out, _ := c.Output()
	return string(out)
}

func gitDiffColour(root, ref, path string) string {
	c := exec.Command("git", "diff", "--color=always", ref, "--", path)
	c.Dir = root
	out, _ := c.Output()
	return string(out)
}

func pipeToDelta(input string, width int) (string, error) {
	c := exec.Command("delta", "--paging=never",
		"--width", strconv.Itoa(width))
	c.Stdin = strings.NewReader(input)
	out, err := c.Output()
	return string(out), err
}

func haveBin(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func dim(s string) string {
	return "\033[2m" + s + "\033[0m"
}
