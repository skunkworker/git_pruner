package main

import (
	"os"
	"path/filepath"
	"strings"
)

// settingsPath is where the sort choice is kept between runs. main sets it;
// it stays "" under test, so no test reads or writes the user's real file.
var settingsPath string

// defaultSettingsPath is <user config dir>/git_pruner/settings, or "" when the
// system has no config dir.
func defaultSettingsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "git_pruner", "settings")
}

// loadSettings applies the saved sort field and order. A missing or broken file
// leaves the defaults: this is a convenience, never a reason to fail startup.
func loadSettings(m *model) {
	if settingsPath == "" {
		return
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, val, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch key {
		case "sort":
			for f := range sortFieldCount {
				if f.String() == val {
					m.field = f
				}
			}
		case "order":
			m.ascending = val == "asc"
		}
	}
}

// saveSettings writes the sort field and order. Errors are ignored for the
// same reason loadSettings ignores them.
func saveSettings(m model) {
	if settingsPath == "" {
		return
	}
	order := "desc"
	if m.ascending {
		order = "asc"
	}
	_ = os.MkdirAll(filepath.Dir(settingsPath), 0o755)
	_ = os.WriteFile(settingsPath, []byte("sort="+m.field.String()+"\norder="+order+"\n"), 0o644)
}
