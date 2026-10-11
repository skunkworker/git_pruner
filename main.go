package main

import (
	"fmt"
	"os"
	"runtime/debug"

	tea "github.com/charmbracelet/bubbletea"
)

// Build metadata. version, buildTime and gitCommit can be injected at link time via
// -ldflags "-X main.version=... -X main.buildTime=... -X main.gitCommit=..." (or
// -X main.buildDate=...); when unset, buildInfo falls back to the VCS data the Go
// toolchain embeds.
var (
	version   string
	buildTime string
	buildDate string
	gitCommit string
)

// buildInfo returns the version (git tag), the commit the binary was built from,
// and when it was compiled, preferring -ldflags values and falling back to Go's
// embedded VCS info.
func buildInfo() (ver, commit, date string) {
	ver = version
	commit = gitCommit
	date = buildTime
	if date == "" {
		date = buildDate
	}
	dirty := false
	if info, ok := debug.ReadBuildInfo(); ok {
		// go build stamps this from git tags: "v0.1.0" on a tagged commit, a
		// "v0.1.1-0.<time>-<hash>" pseudo-version on later commits, "+dirty" with
		// uncommitted changes, and "(devel)" when it has no VCS info.
		if ver == "" && info.Main.Version != "(devel)" {
			ver = info.Main.Version
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if commit == "" {
					commit = s.Value
				}
			case "vcs.time":
				if date == "" {
					date = s.Value
				}
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	if ver == "" {
		ver = "unknown"
	}
	if commit == "" {
		commit = "unknown"
	}
	if date == "" {
		date = "unknown"
	}
	if len(commit) > 7 && commit != "unknown" {
		commit = commit[:7]
	}
	if dirty {
		commit += " (dirty)"
	}
	return ver, commit, date
}

// versionString reports the build's version, commit, date, and Go version.
// Fields fall back to "unknown" when build info is unavailable (e.g. `go run`).
func versionString() string {
	ver, commit, date := buildInfo()
	goVer := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		goVer = info.GoVersion
	}
	return fmt.Sprintf("git_pruner\n  version: %s\n  commit:  %s\n  date:    %s\n  go:      %s", ver, commit, date, goVer)
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println(versionString())
			return
		}
		os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
	}

	settingsPath = defaultSettingsPath()
	m, err := startupModel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "git_pruner:", err)
		os.Exit(1)
	}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	final, err := p.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "git_pruner:", err)
		os.Exit(1)
	}
	// Printed after the alt screen closes, so the commands stay in scrollback.
	if fm, ok := final.(model); ok {
		fmt.Print(restoreSummary(append(fm.history, fm.results...)))
	}
}
