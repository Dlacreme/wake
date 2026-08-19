package main

import (
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/Dlacreme/wake/config"
	"github.com/Dlacreme/wake/gh"
	"github.com/Dlacreme/wake/git"
	"github.com/Dlacreme/wake/ui"
)

const usage = `wake — review a changeset you didn't write

Usage:
  wake                          changes vs HEAD (staged + unstaged + untracked)
  wake --since <ref>            everything on this branch since <ref>
  wake --since HEAD~3           last three commits plus working tree
  wake --pr <number>            review a GitHub PR by number
  wake --pr <url>               review a GitHub PR by URL

Keys:
  enter       open in $EDITOR at the first changed hunk
  v           mark file viewed (hides it until its diff changes)
  n           add/edit a note on the current file
  N           view all pending notes
  ctrl-d      toggle diff / whole-file view
  ctrl-g      grep the changed files only
  ctrl-f      back to the changed-file list
  ctrl-p      peek: fuzzy-find any file in the repo
  ctrl-r      refresh
  ctrl-s      publish notes as PR review (--pr mode only)
  ctrl-/      cycle preview layout
  q / ctrl-c  quit

Config files (flat TOML):
  ~/.config/wake/config.toml   user-level
  .wake.toml                   project-level (repo root)

Config keys: editor, editor_line_fmt, since, preview, preview_width, sort, exclude
`

func main() {
	since, prRef, showHelp := parseArgs(os.Args[1:])

	if showHelp {
		fmt.Print(usage)
		os.Exit(0)
	}

	root, err := git.Root()
	if err != nil {
		die(err.Error())
	}

	cfg := config.Load(root)

	// CLI flags override config/env
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

	// validate git.ChangedItems can run (error early on bad state)
	// skip in PR mode — the PR diff is the file source, not local changes
	if prRef == "" {
		if _, err := git.ChangedItems(root, since, cfg.Exclude, cfg.Sort); err != nil {
			die(err.Error())
		}
	}

	// parse PR ref if provided
	var pr *gh.PR
	if prRef != "" {
		parsed, err := gh.Parse(prRef, root)
		if err != nil {
			die(err.Error())
		}
		pr = &parsed
	}

	m := ui.New(root, since, cfg, pr)
	p := tea.NewProgram(m,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	if _, err := p.Run(); err != nil {
		die(err.Error())
	}
}

func parseArgs(args []string) (since, prRef string, help bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			help = true
		case arg == "--since":
			if i+1 < len(args) {
				since = args[i+1]
				i++
			} else {
				die("--since requires a ref argument")
			}
		case strings.HasPrefix(arg, "--since="):
			since = arg[len("--since="):]
		case arg == "--pr":
			if i+1 < len(args) {
				prRef = args[i+1]
				i++
			} else {
				die("--pr requires a PR number or URL")
			}
		case strings.HasPrefix(arg, "--pr="):
			prRef = arg[len("--pr="):]
		default:
			die("unknown option: " + arg + " (try --help)")
		}
	}
	return since, prRef, help
}

func die(msg string) {
	fmt.Fprintf(os.Stderr, "wake: %s\n", msg)
	os.Exit(1)
}
