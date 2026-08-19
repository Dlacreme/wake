# wake

Review a changeset you didn't write.

A terminal navigator for changed files. Start from what moved, jump to your editor at the right line, search only what changed. Works on any branch or working tree — not just AI output.

```
┌─────────────────────────────────────────────────────────┐
│ changed>                                    6/6   wake  │
├──────────────────┬──────────────────────────────────────┤
│ M  modified.txt  │ @@ -1,3 +1,3 @@                     │
│ A  new-file.txt  │ -original line                       │
│ D  deleted.txt   │ +changed line                        │
│ R  renamed.txt   │  second line                         │
├──────────────────┴──────────────────────────────────────┤
│ enter open · v viewed · ctrl-d diff/full · ctrl-p peek  │
└─────────────────────────────────────────────────────────┘
```

## Install

**Requires Go 1.21+.**

```bash
go install github.com/Dlacreme/wake@latest
```

The binary lands in `$GOPATH/bin` (usually `~/go/bin`). Make sure that's on your `$PATH`.

**Optional but recommended** — wake uses these when present:

```bash
brew install bat delta ripgrep   # macOS
# apt install bat ripgrep        # Debian/Ubuntu (delta: see dandavison/delta)
```

### From source

```bash
git clone https://github.com/Dlacreme/wake
cd wake
make install   # runs go install, puts wake in ~/go/bin
```

## Usage

```bash
wake                  # staged + unstaged + untracked vs HEAD
wake --since main     # everything on this branch since main
wake --since HEAD~3   # last three commits plus working tree
```

## Keys

| Key | Action |
|-----|--------|
| `enter` | Open in `$EDITOR` at the first changed hunk |
| `v` | Mark viewed — hides the file until its diff changes |
| `ctrl-d` | Toggle diff / whole-file view |
| `ctrl-g` | Grep the changed files only |
| `ctrl-f` | Back to the changed-file list |
| `ctrl-p` | Peek — fuzzy-find any file in the repo, `esc` to return |
| `ctrl-r` | Refresh |
| `ctrl-/` | Cycle preview layout (right → bottom → hidden) |
| `↑/k` `↓/j` | Navigate |
| `q` | Quit |

## Config

Two locations, both optional:

```
~/.config/wake/config.toml   # user-level
.wake.toml                   # project-level (repo root)
```

Project config takes precedence over user config. CLI flags take precedence over both.

```toml
editor         = "nvim"
preview        = "diff"       # "diff" or "full" — default view mode
preview_width  = 62           # preview pane width, percent
sort           = "alpha"      # "alpha" or "mtime"
exclude        = ["*.lock", "dist/**", "*.snap"]
```

All keys and their defaults:

| Key | Default | Description |
|-----|---------|-------------|
| `editor` | `$EDITOR` | Editor binary |
| `editor_line_fmt` | — | Custom line-jump format, e.g. `--goto {file}:{line}` |
| `since` | — | Default base ref (same as `--since`) |
| `preview` | `diff` | Default preview mode |
| `preview_width` | `62` | Preview pane width (percent) |
| `sort` | `alpha` | `alpha` or `mtime` (most recently modified first) |
| `exclude` | — | Glob patterns to hide from the list |

## Editor setup

wake passes the file and line number to your editor automatically. Supported out of the box: `vim`, `nvim`, `vi`, `code`, `cursor`, `windsurf`, `codium`, `subl`, `hx`, `helix`, `emacs`, `emacsclient`, `nano`, `micro`.

For anything else, set `editor_line_fmt`:

```toml
editor_line_fmt = "--goto {file}:{line}"
```

Or via env:

```bash
export WAKE_EDITOR_LINE_FMT="--goto {file}:{line}"
```
