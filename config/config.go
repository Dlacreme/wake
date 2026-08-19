package config

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config holds all runtime configuration for wake.
// Precedence (highest → lowest):
//   1. CLI flags (applied by main after Load)
//   2. Env vars  (WAKE_SINCE, WAKE_EDITOR_LINE_FMT, EDITOR)
//   3. .wake.toml in repo root
//   4. ~/.config/wake/config.toml
//   5. Hardcoded defaults below
type Config struct {
	Editor        string   `toml:"editor"`
	EditorLineFmt string   `toml:"editor_line_fmt"`
	Since         string   `toml:"since"`
	Preview       string   `toml:"preview"`       // "diff" | "full"
	PreviewWidth  int      `toml:"preview_width"` // percent of terminal width
	Sort          string   `toml:"sort"`          // "mtime" only for now
	Exclude       []string `toml:"exclude"`       // glob patterns
}

func defaults() Config {
	return Config{
		Editor:       "vim",
		Preview:      "diff",
		PreviewWidth: 62,
		Sort:         "mtime",
	}
}

// Load reads config files and env vars, returning a merged Config.
// repoRoot is the git repo root (for .wake.toml lookup); may be empty.
func Load(repoRoot string) Config {
	cfg := defaults()

	// 4. user config
	if home, err := os.UserHomeDir(); err == nil {
		userPath := filepath.Join(home, ".config", "wake", "config.toml")
		_ = merge(&cfg, userPath)
	}

	// 3. project config
	if repoRoot != "" {
		_ = merge(&cfg, filepath.Join(repoRoot, ".wake.toml"))
	}

	// 2. env vars
	if v := os.Getenv("EDITOR"); v != "" {
		cfg.Editor = v
	}
	if v := os.Getenv("WAKE_EDITOR_LINE_FMT"); v != "" {
		cfg.EditorLineFmt = v
	}
	if v := os.Getenv("WAKE_SINCE"); v != "" {
		cfg.Since = v
	}

	return cfg
}

// merge reads a TOML file into cfg, skipping missing files silently.
func merge(cfg *Config, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err // file absent — normal
	}
	defer f.Close()

	var file Config
	if _, err := toml.NewDecoder(f).Decode(&file); err != nil {
		return err
	}

	if file.Editor != "" {
		cfg.Editor = file.Editor
	}
	if file.EditorLineFmt != "" {
		cfg.EditorLineFmt = file.EditorLineFmt
	}
	if file.Since != "" {
		cfg.Since = file.Since
	}
	if file.Preview != "" {
		cfg.Preview = file.Preview
	}
	if file.PreviewWidth != 0 {
		cfg.PreviewWidth = file.PreviewWidth
	}
	if file.Sort != "" {
		cfg.Sort = file.Sort
	}
	if len(file.Exclude) > 0 {
		cfg.Exclude = file.Exclude
	}

	return nil
}
