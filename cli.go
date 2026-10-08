package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// runCLI is script mode: the TUI's selection rules, without the TUI. It only
// deletes with --yes, so a cron line or a typo cannot delete by accident. It
// never deletes remote branches: that stays a deliberate key in the TUI.
// Returns the exit code.
func runCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("git_pruner", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gone := fs.Bool("prune-gone", false, "select gone branches that hold no commits missing from the default branch")
	olderThan := fs.Int("merged-older-than", -1, "select merged branches whose last commit is older than `DAYS`")
	fetch := fs.Bool("fetch", false, "run git fetch --all --prune first")
	yes := fs.Bool("yes", false, "delete the selected branches (without it, only list them)")
	dryRun := fs.Bool("dry-run", false, "only list what would be deleted, even with --yes")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: git_pruner [--fetch] (--prune-gone | --merged-older-than DAYS)... [--yes | --dry-run]")
		fmt.Fprintln(stderr, "       git_pruner            start the interactive screen")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || (!*gone && *olderThan < 0) {
		fs.Usage()
		return 2
	}

	if *fetch {
		if _, err := runGit("fetch", "--all", "--prune"); err != nil {
			fmt.Fprintln(stderr, "git_pruner: fetch:", err)
			return 1
		}
	}
	m, err := initialModel()
	if err != nil {
		fmt.Fprintln(stderr, "git_pruner:", err)
		return 1
	}
	if *gone {
		fmt.Fprintln(stderr, m.selectGone())
	}
	if *olderThan >= 0 {
		fmt.Fprintln(stderr, m.selectMergedOlder(*olderThan, time.Now()))
	}
	m.measureSelectedRisk()
	sel := m.selectedBranches()
	if len(sel) == 0 {
		fmt.Fprintln(stdout, "nothing to delete")
		return 0
	}

	if *dryRun || !*yes {
		for _, b := range sel {
			line := "would delete " + b.name + " (" + b.deleteFlag(false) + ")"
			if w := m.worktreeWarning(b); w != "" {
				line += "  " + w
			}
			if w := m.riskWarning(b); w != "" {
				line += "  " + w
			}
			fmt.Fprintln(stdout, line)
		}
		if !*dryRun {
			fmt.Fprintln(stderr, "rerun with --yes to delete")
		}
		return 0
	}

	// runGit caps the concurrency, so one goroutine per branch is safe.
	results := make([]deleteResult, len(sel))
	var wg sync.WaitGroup
	for i, b := range sel {
		wg.Go(func() { results[i] = deleteBranch(b, false, false) })
	}
	wg.Wait()

	code := 0
	for _, r := range results {
		if r.worktreeRemoved {
			fmt.Fprintf(stdout, "removed worktree %s\n", r.br.worktree)
		}
		if r.localOK {
			fmt.Fprintf(stdout, "deleted %s (was %s)\n", r.br.name, r.br.hash)
		} else {
			fmt.Fprintf(stdout, "failed %s: %s\n", r.br.name, r.localErr)
			code = 1
		}
	}
	fmt.Fprint(stderr, restoreSummary(results))
	return code
}

// restoreSummary lists a command that brings back each branch deleted this
// session. The TUI prints it on quit and script mode after a run, so the way
// back stays in the terminal's scrollback after the screen is gone.
func restoreSummary(results []deleteResult) string {
	var lines []string
	for _, r := range results {
		if r.restorable() {
			lines = append(lines, fmt.Sprintf("  git branch %s %s", r.br.name, r.br.sha))
		}
		if r.remoteOK && r.br.sha != "" {
			lines = append(lines, fmt.Sprintf("  git push %s %s:refs/heads/%s", r.br.remoteName(), r.br.sha, r.br.remoteBranch()))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "git_pruner: to restore a deleted branch, run:\n" + strings.Join(lines, "\n") + "\n"
}
