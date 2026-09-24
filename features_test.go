package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// A remote that never answers must fail within netTimeout, not hang the TUI,
// and git must be told never to prompt. A fake git on PATH stands in for the
// hung remote: it records the prompt setting and then sleeps.
func TestNetworkCallTimesOut(t *testing.T) {
	realGit := mustLookPath(t, "git")
	bin := t.TempDir()
	seen := filepath.Join(bin, "prompt")
	script := "#!/bin/sh\nif [ \"$1\" = fetch ]; then echo \"$GIT_TERMINAL_PROMPT\" > " + seen +
		"; exec sleep 30; fi\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := netTimeout
	netTimeout = 300 * time.Millisecond
	t.Cleanup(func() { netTimeout = old })

	start := time.Now()
	_, err := runGit("fetch", "--all")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want a timeout error, got %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the call took %s; the timeout did not stop it", d)
	}
	if got, _ := os.ReadFile(seen); strings.TrimSpace(string(got)) != "0" {
		t.Fatalf("GIT_TERMINAL_PROMPT must be 0, got %q", got)
	}
	// Local calls have no time limit and are not affected.
	if _, err := runGit("version"); err != nil {
		t.Fatal(err)
	}
}

// Commit subjects come from other people. An escape code in one must not reach
// the terminal, where it could recolor or move the screen.
func TestSubjectEscapeCodesAreRemoved(t *testing.T) {
	repo := initRepo(t, "main")
	chdir(t, repo)
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "red \x1b[31mtext\x07 here")
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	b := find(m.branches, "main")
	if b.subject != "red text here" {
		t.Fatalf("subject must be cleaned, got %q", b.subject)
	}
	if row := m.renderRow(*b, 10, false); strings.Contains(row, "\x1b[31m") {
		t.Fatalf("the escape code reached the row: %q", row)
	}
	if got := cleanText("a\tb\nc\x1b[2Jd"); got != "a b cd" {
		t.Fatalf("cleanText: got %q", got)
	}
}

// u recreates what the last run deleted — at the same commit, with the upstream
// config `git branch -d` removed.
func TestUndoRestoresDeletedBranches(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	sha := git(t, repo, "rev-parse", "feature/tracked")
	find(m.branches, "feature/tracked").selected = true
	m.force = true
	m = startAndDrain(t, m, false)
	wantState(t, m, stateResult, "a clean -D run lands on the results screen")
	if find(m.branches, "feature/tracked") != nil {
		t.Fatal("precondition: the branch must be deleted")
	}
	if o := stripANSI(m.resultView()); !strings.Contains(o, "u undo") {
		t.Fatalf("the results screen must offer undo:\n%s", o)
	}
	summary := restoreSummary(m.results)
	if !strings.Contains(summary, "git branch feature/tracked "+sha) {
		t.Fatalf("the exit summary must give the restore command:\n%s", summary)
	}

	nm, cmd := m.Update(key("u"))
	m = nm.(model)
	wantState(t, m, stateList, "undo returns to the list")
	if cmd == nil {
		t.Fatal("u must start the restore")
	}
	nm, _ = m.Update(cmd())
	m = nm.(model)
	if got := git(t, repo, "rev-parse", "refs/heads/feature/tracked"); got != sha {
		t.Fatalf("restored at %s, want %s", got, sha)
	}
	if up := git(t, repo, "rev-parse", "--abbrev-ref", "feature/tracked@{upstream}"); up != "origin/feature/tracked" {
		t.Fatalf("upstream must be restored, got %q", up)
	}
	if !strings.Contains(m.status, "restored 1 branch") {
		t.Fatalf("status: %q", m.status)
	}
	if restoreSummary(m.results) != "" {
		t.Fatal("a restored branch needs no restore command")
	}
	// A second u has nothing left to do.
	if m = press(t, m, key("u")); m.status != "nothing to undo" {
		t.Fatalf("status: %q", m.status)
	}
}

// The force prompt says how to get the commits back.
func TestForcePromptGivesRecoveryHint(t *testing.T) {
	m := model{height: 40, width: 120, riskBase: "main", results: []deleteResult{
		{br: branch{name: "wip", hash: "abc1234", riskCommits: 2}, done: true, forceable: true},
	}}
	o := stripANSI(m.forcePromptView())
	for _, want := range []string{"press u", "git branch <name> <commit>", "wip  at abc1234"} {
		if !strings.Contains(o, want) {
			t.Fatalf("missing %q:\n%s", want, o)
		}
	}
}

// m selects the listed branches that are merged and older than N days — never
// a young one, an unmerged one, or a locked one.
func TestSelectMergedOlder(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	old, young := now.AddDate(0, 0, -100), now.AddDate(0, 0, -10)
	m := model{width: 100, height: 24,
		baseMerged: map[string]bool{"old-merged": true, "young-merged": true, "main": true},
		branches: []branch{
			{name: "main", committed: old, protected: true},
			{name: "old-merged", committed: old},
			{name: "young-merged", committed: young},
			{name: "old-unmerged", committed: old},
			{name: "old-upstream-merged", committed: old, upstream: "origin/x", remoteMerged: true},
			{name: "old-ahead", committed: old, upstream: "origin/y", remoteMerged: true, ahead: 1},
		}}
	m = press(t, m, key("m"))
	if m.state != stateAge || m.ageInput != "0" {
		// config is zero-valued here; startup fills it from pruner.staleDays
		t.Fatalf("m must open the age prompt, state %v input %q", m.state, m.ageInput)
	}
	m.status = m.selectMergedOlder(90, now)
	want := map[string]bool{"old-merged": true, "old-upstream-merged": true}
	for _, b := range m.branches {
		if b.selected != want[b.name] {
			t.Fatalf("%s selected=%v, want %v", b.name, b.selected, want[b.name])
		}
	}
	if m.status != "selected 2 merged branch(es) older than 90 days" {
		t.Fatalf("status: %q", m.status)
	}

	// The prompt takes digits only, and enter applies it.
	m = press(t, model{state: stateAge, ageInput: "9"}, key("x"), key("0"), special(tea.KeyEnter))
	if m.state != stateList || !strings.Contains(m.status, "older than 90 days") {
		t.Fatalf("prompt: state %v status %q", m.state, m.status)
	}
}

// No row may be wider than the terminal: a wrapped row takes two screen lines
// that visibleRows does not count.
func TestRowsFitTheTerminal(t *testing.T) {
	br := branch{name: "feature/a-very-long-branch-name-here", upstream: "origin/x", ahead: 3, behind: 12,
		remoteMerged: true, hash: "abc1234", committedRel: "3 weeks ago", subject: strings.Repeat("subject ", 20)}
	for w := 30; w <= 200; w += 7 {
		m := model{width: w}
		for _, cursor := range []bool{false, true} {
			if got := ansi.StringWidth(m.renderRow(br, 36, cursor)); got > w {
				t.Fatalf("width %d: row is %d cells", w, got)
			}
		}
	}
	// At 80 columns the relative date goes first, and a subject still shows.
	row := stripANSI(model{width: 80}.renderRow(br, 20, false))
	if strings.Contains(row, "3 weeks ago") || !strings.Contains(row, "abc1234") || !strings.Contains(row, "subject") {
		t.Fatalf("80 columns: %q", row)
	}
}

// The list fills the terminal exactly, whatever status and error lines are set.
func TestListViewFitsHeight(t *testing.T) {
	m := model{width: 100, height: 24}
	for i := 0; i < 50; i++ {
		m.branches = append(m.branches, branch{name: "b" + strings.Repeat("x", i%7)})
	}
	for _, c := range []struct{ status, err string }{{"", ""}, {"s", ""}, {"s", "e"}} {
		m.status, m.err = c.status, c.err
		m.cursor = 49
		m.adjustScroll()
		if rows := screenRows(m.listView()); rows != m.height {
			t.Fatalf("status=%q err=%q: %d rows for a %d-row terminal", c.status, c.err, rows, m.height)
		}
		if !strings.Contains(stripANSI(m.listView()), "> ") {
			t.Fatal("the cursor row must stay visible")
		}
	}
}

// The cursor starts on the first row, not wherever the alphabetically first
// branch lands after the date sort.
func TestCursorStartsOnFirstRow(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	if m.cursor != 0 {
		t.Fatalf("cursor starts on row %d", m.cursor)
	}
}

// A gone branch that holds nothing new must not be flagged "not merged": its
// upstream is gone, so there is nothing for remoteMerged to test.
func TestGoneMergedBranchIsNotFlaggedUnmerged(t *testing.T) {
	m := model{width: 100, height: 40, remoteDefault: "origin/main", riskBase: "origin/main", branches: []branch{
		{name: "done", upstream: "origin/done", gone: true, riskMeasured: true, selected: true},
	}}
	o := stripANSI(m.confirmView())
	if strings.Contains(o, "not merged") {
		t.Fatalf("a clean gone branch is flagged:\n%s", o)
	}
	if !strings.Contains(o, "✓ no commits missing from origin/main") {
		t.Fatalf("the clean state must be stated:\n%s", o)
	}
}

// A branch checked out in another worktree is marked + and cannot be selected;
// git would refuse to delete it anyway.
func TestWorktreeBranchIsLocked(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	git(t, repo, "worktree", "add", "-q", filepath.Join(t.TempDir(), "wt"), "feature/unmerged")
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	b := find(m.branches, "feature/unmerged")
	if !b.worktree || b.isCurrent {
		t.Fatalf("want a worktree branch: %+v", b)
	}
	cursorTo(t, &m, "feature/unmerged")
	m = press(t, m, key(" "), key("a"))
	if find(m.branches, "feature/unmerged").selected {
		t.Fatal("a worktree branch must not be selectable")
	}
	if row := stripANSI(m.renderRow(*b, 20, false)); !strings.Contains(row, " + feature/unmerged") {
		t.Fatalf("the row must carry the + marker: %q", row)
	}
}

// The trunk and pruner.protect globs are locked in both views.
func TestProtectedBranches(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	git(t, repo, "config", "--add", "pruner.protect", "feature/m*")
	git(t, repo, "config", "pruner.staleDays", "30")
	git(t, repo, "checkout", "-q", "feature/tracked")
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"main": true, "feature/merged": true, "feature/unmerged": false} {
		if got := find(m.branches, name).protected; got != want {
			t.Fatalf("%s protected=%v, want %v", name, got, want)
		}
	}
	m = press(t, m, key("a"))
	if got := branchNames(m.selectedBranches()); len(got) != 1 || got[0] != "feature/unmerged" {
		t.Fatalf("a must skip protected branches, selected %v", got)
	}
	if m.config.staleDays != 30 {
		t.Fatalf("pruner.staleDays: got %d", m.config.staleDays)
	}
}

// The sort choice survives a restart.
func TestSortSettingPersists(t *testing.T) {
	settingsPath = filepath.Join(t.TempDir(), "d", "settings")
	t.Cleanup(func() { settingsPath = "" })
	m := press(t, model{}, key("s"), key("o"))
	var fresh model
	loadSettings(&fresh)
	if fresh.field != sortName || !fresh.ascending {
		t.Fatalf("loaded %v asc=%v, want %v asc=true (saved as %v)", fresh.field, fresh.ascending, sortName, m.field)
	}
}

// The TUI paints before the merge queries: Init runs them, and the keys that
// select by their answers wait until they land.
func TestStartupMergeInfoArrivesLater(t *testing.T) {
	repo := setupGoneMerged(t, 3)
	chdir(t, repo)
	m, err := startupModel()
	if err != nil {
		t.Fatal(err)
	}
	if !m.merging {
		t.Fatal("startupModel must leave the merge queries to Init")
	}
	m = press(t, m, key("x"))
	if len(m.selectedBranches()) != 0 || !strings.Contains(m.status, "still reading") {
		t.Fatalf("x must wait for the merge info: status %q", m.status)
	}
	msg := m.Init()()
	// A result for an older load is dropped.
	stale := msg.(mergeInfoMsg)
	stale.gen--
	if nm, _ := m.Update(stale); !nm.(model).merging {
		t.Fatal("a stale merge result must be ignored")
	}
	nm, _ := m.Update(msg)
	m = nm.(model)
	if m.merging || !find(m.branches, "feature/0").riskMeasured {
		t.Fatal("the merge info must be applied")
	}
	if m = press(t, m, key("x")); len(m.selectedBranches()) != 3 {
		t.Fatalf("x after the load: %s", m.status)
	}
}

// v loads the diff in the background; an answer for an older request is dropped.
func TestDiffLoadsInBackground(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	// A PATH with git alone, so a delta installed beside it is not used.
	bin := t.TempDir()
	if err := os.Symlink(mustLookPath(t, "git"), filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	cursorTo(t, &m, "feature/unmerged")
	nm, cmd := m.Update(key("v"))
	m = nm.(model)
	if m.state != stateDiff || !m.diffLoading || cmd == nil {
		t.Fatal("v must open the diff view and start the load")
	}
	if o := stripANSI(m.diffView()); !strings.Contains(o, "loading diff") {
		t.Fatalf("the view must say it is loading:\n%s", o)
	}
	msg := cmd().(diffLoadedMsg)
	old := msg
	old.seq--
	old.lines = []string{"stale"}
	nm, _ = m.Update(old)
	if nm.(model).diffLines != nil {
		t.Fatal("a stale diff must be dropped")
	}
	nm, _ = m.Update(msg)
	m = nm.(model)
	if m.diffLoading || !strings.Contains(strings.Join(m.diffLines, "\n"), "+b") || m.diffBase != "origin/main" {
		t.Fatalf("diff not applied: base %q lines %q", m.diffBase, m.diffLines)
	}
}

func mustLookPath(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// setupRemoteOnly adds two branches to origin that no local branch tracks:
// "done" (merged into main) and "orphan" (one commit main lacks).
func setupRemoteOnly(t *testing.T) string {
	t.Helper()
	repo := setupRepo(t)
	git(t, repo, "push", "-q", "origin", "main:refs/heads/done", "feature/unmerged:refs/heads/orphan")
	return repo
}

// tab shows the remote branches no local branch tracks. Deleting them is a push,
// so only R does it; y has nothing to do.
func TestRemoteOnlyView(t *testing.T) {
	repo := setupRemoteOnly(t)
	chdir(t, repo)
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	nm, cmd := m.Update(special(tea.KeyTab))
	m = nm.(model)
	if cmd == nil || !m.showRemote {
		t.Fatal("tab must switch view and load the remote rows")
	}
	nm, _ = m.Update(cmd())
	m = nm.(model)
	var names []string
	for _, i := range m.viewIdx() {
		names = append(names, m.branches[i].name)
	}
	// origin/main is protected, origin/feature/tracked is tracked locally.
	if strings.Join(names, ",") != "origin/done,origin/orphan" && strings.Join(names, ",") != "origin/orphan,origin/done" {
		t.Fatalf("remote-only rows: %v", names)
	}
	if !find(m.branches, "origin/done").remoteMerged || find(m.branches, "origin/orphan").remoteMerged {
		t.Fatal("merge marks on remote rows are wrong")
	}

	m = press(t, m, key("a"), key("d"))
	wantState(t, m, stateConfirm, "d opens the confirm screen")
	o := stripANSI(m.confirmView())
	if !strings.Contains(o, "R = delete 2 remote branch(es)") || !strings.Contains(o, "1 commit(s) not in origin/main") {
		t.Fatalf("confirm screen:\n%s", o)
	}
	if nm, cmd := m.Update(key("y")); cmd != nil || nm.(model).state != stateConfirm {
		t.Fatal("y must not delete remote-only rows")
	}
	nm, cmd = m.Update(key("R"))
	m = drainDeletions(t, nm.(model), cmd)
	for _, name := range []string{"done", "orphan"} {
		if remoteHasBranch(t, repo, name) {
			t.Fatalf("origin still has %s", name)
		}
	}
	if !strings.Contains(restoreSummary(m.results), "git push origin ") {
		t.Fatal("the exit summary must say how to push a deleted remote branch back")
	}
}

// c on a remote-only row creates a local branch that tracks it.
func TestCheckoutRemoteOnly(t *testing.T) {
	repo := setupRemoteOnly(t)
	chdir(t, repo)
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	nm, cmd := m.Update(special(tea.KeyTab))
	nm, _ = nm.(model).Update(cmd())
	m = nm.(model)
	cursorTo(t, &m, "origin/orphan")
	m = doSwitch(t, m)
	if m.err != "" || currentBranch(t, repo) != "orphan" {
		t.Fatalf("switch failed: %q", m.err)
	}
	if m.showRemote {
		t.Fatal("the list must move to the local view")
	}
	if b := m.cur(); b == nil || b.name != "orphan" || b.upstream != "origin/orphan" {
		t.Fatalf("cursor must be on the new tracking branch: %+v", b)
	}
}

// Script mode lists without --yes, deletes with it, and keeps the same safety
// rules as the TUI.
func TestScriptMode(t *testing.T) {
	repo := setupGoneMerged(t, 2)
	chdir(t, repo)
	git(t, repo, "checkout", "-q", "-b", "wip", "main")
	commitFile(t, repo, "w", "w")
	git(t, repo, "config", "branch.wip.remote", "origin")
	git(t, repo, "config", "branch.wip.merge", "refs/heads/wip")
	git(t, repo, "checkout", "-q", "main")

	var out, errOut bytes.Buffer
	if code := runCLI([]string{"--prune-gone"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "would delete feature/0 (-D)") || strings.Contains(out.String(), "wip") {
		t.Fatalf("dry run:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "1 hold commits not in main") {
		t.Fatalf("the risky branch must be reported:\n%s", errOut.String())
	}
	if find(mustLoad(t), "feature/0") == nil {
		t.Fatal("a dry run must delete nothing")
	}

	out.Reset()
	errOut.Reset()
	if code := runCLI([]string{"--prune-gone", "--yes"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	bs := mustLoad(t)
	if find(bs, "feature/0") != nil || find(bs, "feature/1") != nil || find(bs, "wip") == nil {
		t.Fatalf("after --yes: %v", branchNames(bs))
	}
	if !strings.Contains(errOut.String(), "git branch feature/0 ") {
		t.Fatalf("restore commands must be printed:\n%s", errOut.String())
	}

	if code := runCLI([]string{"--bogus"}, &out, &errOut); code != 2 {
		t.Fatalf("a bad flag must exit 2, got %d", code)
	}
	if code := runCLI(nil, &out, &errOut); code != 2 {
		t.Fatalf("no rule must exit 2, got %d", code)
	}
}

func mustLoad(t *testing.T) []branch {
	t.Helper()
	bs, err := loadBranches()
	if err != nil {
		t.Fatal(err)
	}
	return bs
}
