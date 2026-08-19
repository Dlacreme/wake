package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/Dlacreme/wake/config"
	"github.com/Dlacreme/wake/git"
	"github.com/Dlacreme/wake/ui"
)

const usage = `wake — review a changeset you didn't write

Usage:
  wake                   changes vs HEAD (staged + unstaged + untracked)
  wake --since <ref>     everything on this branch since <ref>
  wake --since HEAD~3    last three commits plus working tree

Keys:
  enter     open in $EDITOR at the first changed hunk
  v         mark file viewed (hides it until its diff changes)
  ctrl-d    toggle diff / whole-file view
  ctrl-g    grep the changed files only
  ctrl-f    back to the changed-file list
  ctrl-p    peek: fuzzy-find any file in the repo
  ctrl-r    refresh
  ctrl-/    cycle preview layout
  q / ctrl-c  quit

Config files (flat TOML):
  ~/.config/wake/config.toml   user-level
  .wake.toml                   project-level (repo root)

Keys: editor, editor_line_fmt, since, preview, preview_width, sort, exclude
`

func main() {
	since, showHelp := parseArgs(os.Args[1:])

	if showHelp {
		fmt.Print(usage)
		os.Exit(0)
	}

	root, err := git.Root()
	if err != nil {
		die(err.Error())
	}

	cfg := config.Load(root)

	// CLI --since overrides config/env
	if since != "" {
		cfg.Since = since
	}
	since = cfg.Since

	// validate ref if provided
	if since != "" {
		if err := git.ValidateRef(root, since); err != nil {
			die(err.Error())
		}
	}

	// fast-path: nothing changed?
	items, err := git.ChangedItems(root, since, cfg.Exclude)
	if err != nil {
		die(err.Error())
	}
	if len(items) == 0 {
		msg := "wake: nothing changed"
		if since != "" {
			msg += " since " + since
		}
		fmt.Println(msg + ".")
		os.Exit(0)
	}

	m := ui.New(root, since, cfg)
	p := tea.NewProgram(m,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	if _, err := p.Run(); err != nil {
		die(err.Error())
	}
}

func parseArgs(args []string) (since string, help bool) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			help = true
		case "--since":
			if i+1 < len(args) {
				since = args[i+1]
				i++
			} else {
				die("--since requires a ref argument")
			}
		default:
			// handle --since=ref
			if len(args[i]) > 8 && args[i][:8] == "--since=" {
				since = args[i][8:]
			} else {
				die("unknown option: " + args[i] + " (try --help)")
			}
		}
	}
	return since, help
}

func die(msg string) {
	fmt.Fprintf(os.Stderr, "wake: %s\n", msg)
	os.Exit(1)
}
