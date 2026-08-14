package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// key builds a rune KeyMsg (e.g. "y", "R") for driving update handlers in tests.
func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// runCmd executes a tea.Cmd to completion, recursing into batched cmds, so tests
// can drive the async deletion path synchronously.
func runCmd(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	if batch, ok := cmd().(tea.BatchMsg); ok {
		for _, c := range batch {
			runCmd(t, c)
		}
	}
}

// remoteHasBranch reports whether origin still has the named branch.
func remoteHasBranch(t *testing.T, dir, name string) bool {
	t.Helper()
	cmd := exec.Command("git", "ls-remote", "--heads", "origin", name)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ls-remote: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out)) != ""
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func setupRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	remote := t.TempDir()
	git(t, remote, "init", "--bare", "-q")
	git(t, tmp, "init", "-q", "-b", "main")
	git(t, tmp, "config", "user.email", "t@t.t")
	git(t, tmp, "config", "user.name", "t")
	commitFile(t, tmp, "a", "a")
	git(t, tmp, "remote", "add", "origin", remote)
	git(t, tmp, "push", "-q", "-u", "origin", "main")
	git(t, tmp, "branch", "feature/merged") // merged into main -> safe delete
	git(t, tmp, "checkout", "-q", "-b", "feature/unmerged")
	commitFile(t, tmp, "b", "b")
	git(t, tmp, "checkout", "-q", "-b", "feature/tracked")
	git(t, tmp, "push", "-q", "-u", "origin", "feature/tracked")
	git(t, tmp, "checkout", "-q", "main")
	return tmp
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

func find(bs []branch, name string) *branch {
	for i := range bs {
		if bs[i].name == name {
			return &bs[i]
		}
	}
	return nil
}

func TestLoadAndSort(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatalf("initialModel: %v", err)
	}
	if len(m.branches) != 4 {
		t.Fatalf("want 4 branches, got %d", len(m.branches))
	}
	if b := find(m.branches, "main"); b == nil || !b.isCurrent {
		t.Fatalf("main should be current: %+v", b)
	}
	if b := find(m.branches, "feature/tracked"); b == nil || b.upstream != "origin/feature/tracked" {
		t.Fatalf("feature/tracked upstream wrong: %+v", b)
	}
	if b := find(m.branches, "feature/merged"); b == nil || b.upstream != "" {
		t.Fatalf("feature/merged should have no upstream: %+v", b)
	}

	// default sort is committerdate descending; flip to name asc and verify order.
	m.field = sortName
	m.ascending = true
	m.sortBranches()
	got := make([]string, len(m.branches))
	for i, b := range m.branches {
		got[i] = b.name
	}
	want := []string{"feature/merged", "feature/tracked", "feature/unmerged", "main"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("name sort: got %v want %v", got, want)
		}
	}

	// View should not panic and should render branch names.
	out := m.listView()
	if !strings.Contains(out, "feature/merged") {
		t.Fatalf("listView missing branch name:\n%s", out)
	}
}

func TestLoadDiff(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	// feature/unmerged adds file "b" relative to main.
	diff, _, err := loadDiff("feature/unmerged")
	if err != nil {
		t.Fatalf("loadDiff: %v", err)
	}
	if !strings.Contains(diff, "+++ b/b") || !strings.Contains(diff, "+b") {
		t.Fatalf("diff should show added file b:\n%s", diff)
	}

	// main vs the default base should be empty.
	if d, _, err := loadDiff("main"); err != nil || strings.TrimSpace(d) != "" {
		t.Fatalf("main diff should be empty, err=%v out=%q", err, d)
	}
}

func TestPruneGoneBranch(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	// Delete the remote branch, then prune so the local tracking ref goes "gone".
	git(t, repo, "push", "-q", "origin", "--delete", "feature/tracked")
	git(t, repo, "fetch", "-q", "--all", "--prune")

	branches, err := loadBranches()
	if err != nil {
		t.Fatalf("loadBranches: %v", err)
	}
	tb := find(branches, "feature/tracked")
	if tb == nil || !tb.gone {
		t.Fatalf("feature/tracked should be gone after remote delete + prune: %+v", tb)
	}

	// A gone branch must prune even in safe mode (-d would refuse it).
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	find(m.branches, "feature/tracked").selected = true
	m.force = false
	m.performDeletions()

	if len(m.results) != 1 || !m.results[0].localOK {
		t.Fatalf("gone branch should force-delete: %+v", m.results)
	}
	if find(m.branches, "feature/tracked") != nil {
		t.Fatal("feature/tracked should be gone after prune")
	}
}

func TestRemoteHelpers(t *testing.T) {
	b := branch{name: "feature/tracked", upstream: "origin/feature/tracked"}
	if b.remoteName() != "origin" {
		t.Fatalf("remoteName: %s", b.remoteName())
	}
	if b.remoteBranch() != "feature/tracked" {
		t.Fatalf("remoteBranch: %s", b.remoteBranch())
	}
	nb := branch{name: "local-only"}
	if nb.remoteName() != "origin" || nb.remoteBranch() != "local-only" {
		t.Fatalf("fallbacks wrong: %s %s", nb.remoteName(), nb.remoteBranch())
	}
}

func TestSafeDeleteRefusesUnmerged(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	find(m.branches, "feature/merged").selected = true
	find(m.branches, "feature/unmerged").selected = true
	m.force = false // safe -d
	m.performDeletions()

	var merged, unmerged *deleteResult
	for i := range m.results {
		switch m.results[i].br.name {
		case "feature/merged":
			merged = &m.results[i]
		case "feature/unmerged":
			unmerged = &m.results[i]
		}
	}
	if merged == nil || !merged.localOK {
		t.Fatalf("merged branch should delete safely: %+v", merged)
	}
	if unmerged == nil || unmerged.localOK {
		t.Fatalf("unmerged branch should be refused by -d: %+v", unmerged)
	}
	// after reload, feature/merged gone, feature/unmerged still present
	if find(m.branches, "feature/merged") != nil {
		t.Fatal("feature/merged should be gone after delete")
	}
	if find(m.branches, "feature/unmerged") == nil {
		t.Fatal("feature/unmerged should remain after refused -d")
	}
}

func TestForceDeleteUnmergedRetry(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	find(m.branches, "feature/unmerged").selected = true
	m.force = false // safe -d, which will be refused
	m.performDeletions()

	// The refused unmerged branch should be surfaced for a force prompt, with
	// its ahead count copied onto the result (0 here: it has no upstream).
	failures := m.forceableFailures()
	if len(failures) != 1 || failures[0].br.name != "feature/unmerged" {
		t.Fatalf("want feature/unmerged in forceableFailures, got %+v", failures)
	}
	if failures[0].br.ahead != 0 {
		t.Fatalf("want ahead=0 captured (no upstream), got %+v", failures[0])
	}
	if find(m.branches, "feature/unmerged") == nil {
		t.Fatal("feature/unmerged should still exist before force retry")
	}

	// Answering yes retries with -D and clears the branch.
	m.forceDeleteUnmerged()
	if len(m.forceableFailures()) != 0 {
		t.Fatalf("no failures should remain after force retry: %+v", m.results)
	}
	for _, r := range m.results {
		if r.br.name == "feature/unmerged" && (!r.localOK || r.localErr != "") {
			t.Fatalf("feature/unmerged should be deleted after force retry: %+v", r)
		}
	}
	if find(m.branches, "feature/unmerged") != nil {
		t.Fatal("feature/unmerged should be gone after force retry")
	}
}

// A branch that deletes cleanly must NOT be flagged forceable, so the force
// prompt never appears for successful safe deletes.
func TestNoForcePromptForCleanDelete(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	find(m.branches, "feature/merged").selected = true
	m.force = false
	m.performDeletions()

	if len(m.results) != 1 || !m.results[0].localOK {
		t.Fatalf("merged branch should delete cleanly: %+v", m.results)
	}
	if m.results[0].forceable {
		t.Fatalf("a successful delete must not be forceable: %+v", m.results[0])
	}
	if got := m.forceableFailures(); len(got) != 0 {
		t.Fatalf("no forceable failures expected, got %+v", got)
	}
}

// A gone branch is deleted with -D outright, so a failed gone delete is not
// forceable (there is no stronger flag to escalate to).
func TestGoneFailureNotForceable(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	git(t, repo, "push", "-q", "origin", "--delete", "feature/tracked")
	git(t, repo, "fetch", "-q", "--all", "--prune")

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	tb := find(m.branches, "feature/tracked")
	if tb == nil || !tb.gone {
		t.Fatalf("feature/tracked should be gone: %+v", tb)
	}
	tb.selected = true
	m.force = false // gone branches still use -D
	m.performDeletions()

	if len(m.results) != 1 || !m.results[0].localOK {
		t.Fatalf("gone branch should force-delete: %+v", m.results)
	}
	if m.results[0].forceable {
		t.Fatalf("a gone (-D) delete must not be marked forceable: %+v", m.results[0])
	}
}

// forceDeleteUnmerged is a no-op when nothing is forceable: results and the
// branch list are left untouched.
func TestForceDeleteUnmergedNoop(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	find(m.branches, "feature/merged").selected = true
	m.force = false
	m.performDeletions()
	before := len(m.branches)

	m.forceDeleteUnmerged() // no forceable failures — should change nothing
	if len(m.forceableFailures()) != 0 {
		t.Fatalf("still no failures expected: %+v", m.results)
	}
	if len(m.branches) != before {
		t.Fatalf("branch list should be unchanged, was %d now %d", before, len(m.branches))
	}
	if find(m.branches, "feature/unmerged") == nil {
		t.Fatal("untouched unmerged branch should remain")
	}
}

func TestForceDeleteAndRemote(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	tb := find(m.branches, "feature/tracked")
	tb.selected = true
	tb.deleteRemote = true
	m.force = true // -D, also needed since tracked has its own commit
	m.performDeletions()

	if len(m.results) != 1 {
		t.Fatalf("want 1 result, got %d", len(m.results))
	}
	r := m.results[0]
	if !r.localOK {
		t.Fatalf("force local delete failed: %s", r.localErr)
	}
	if !r.remoteTried || !r.remoteOK {
		t.Fatalf("remote delete failed: tried=%v err=%s", r.remoteTried, r.remoteErr)
	}
	if find(m.branches, "feature/tracked") != nil {
		t.Fatal("feature/tracked should be gone locally")
	}
}

// #1: the async worker deletes a branch and reports a completed result.
func TestDeleteBranchCmd(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	// Local-only safe delete of the merged branch.
	msg := deleteBranchCmd(2, branch{name: "feature/merged"}, "-d", false)()
	dm, ok := msg.(branchDeletedMsg)
	if !ok {
		t.Fatalf("want branchDeletedMsg, got %T", msg)
	}
	if dm.idx != 2 {
		t.Fatalf("idx: got %d want 2", dm.idx)
	}
	if !dm.res.done || !dm.res.localOK || dm.res.remoteTried {
		t.Fatalf("expected done+localOK, no remote: %+v", dm.res)
	}

	// Force delete + remote push of the tracked branch.
	tracked := branch{name: "feature/tracked", upstream: "origin/feature/tracked", ahead: 1}
	dm2 := deleteBranchCmd(0, tracked, "-D", true)().(branchDeletedMsg)
	if !dm2.res.localOK {
		t.Fatalf("force local delete failed: %s", dm2.res.localErr)
	}
	if !dm2.res.remoteTried || !dm2.res.remoteOK {
		t.Fatalf("remote delete failed: tried=%v err=%s", dm2.res.remoteTried, dm2.res.remoteErr)
	}
	if remoteHasBranch(t, repo, "feature/tracked") {
		t.Fatal("remote feature/tracked should be deleted")
	}
}

// #4: on the confirm screen, 'y' deletes locals only while 'R' also deletes the
// armed remote.
func TestConfirmRemoteConfirmationSplit(t *testing.T) {
	// 'y' spares the remote.
	t.Run("y_local_only", func(t *testing.T) {
		repo := setupRepo(t)
		chdir(t, repo)
		m, err := initialModel()
		if err != nil {
			t.Fatal(err)
		}
		tb := find(m.branches, "feature/tracked")
		tb.selected = true
		tb.deleteRemote = true
		m.force = true
		m.state = stateConfirm

		nm, cmd := m.updateConfirm(key("y"))
		if nm.(model).state != stateDeleting {
			t.Fatalf("state should be stateDeleting, got %v", nm.(model).state)
		}
		if cmd == nil {
			t.Fatal("expected a deletion batch cmd")
		}
		runCmd(t, cmd)
		if !remoteHasBranch(t, repo, "feature/tracked") {
			t.Fatal("'y' must not delete the remote branch")
		}
	})

	// 'R' deletes local + remote.
	t.Run("R_local_and_remote", func(t *testing.T) {
		repo := setupRepo(t)
		chdir(t, repo)
		m, err := initialModel()
		if err != nil {
			t.Fatal(err)
		}
		tb := find(m.branches, "feature/tracked")
		tb.selected = true
		tb.deleteRemote = true
		m.force = true
		m.state = stateConfirm

		nm, cmd := m.updateConfirm(key("R"))
		if nm.(model).state != stateDeleting {
			t.Fatalf("state should be stateDeleting, got %v", nm.(model).state)
		}
		runCmd(t, cmd)
		if remoteHasBranch(t, repo, "feature/tracked") {
			t.Fatal("'R' should delete the remote branch")
		}
	})
}

// #5: refreshMergeInfo flags branches whose upstream is merged into the remote
// default, and leaves unmerged / upstream-less branches unflagged.
func TestRemoteMergedIndicator(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	// A remote branch identical to origin/main is merged into it.
	git(t, repo, "branch", "feature/remote-merged", "main")
	git(t, repo, "push", "-q", "-u", "origin", "feature/remote-merged")

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	if m.remoteDefault == "" {
		t.Fatal("remoteDefault should resolve to origin/main")
	}
	if b := find(m.branches, "feature/remote-merged"); b == nil || !b.remoteMerged {
		t.Fatalf("feature/remote-merged should be remoteMerged: %+v", b)
	}
	// feature/tracked carries its own commit → not merged into origin/main.
	if b := find(m.branches, "feature/tracked"); b == nil || b.remoteMerged {
		t.Fatalf("feature/tracked should not be remoteMerged: %+v", b)
	}
	// feature/merged has no upstream → never remoteMerged.
	if b := find(m.branches, "feature/merged"); b == nil || b.remoteMerged {
		t.Fatalf("upstream-less branch must not be remoteMerged: %+v", b)
	}
}

// Scrolling the diff view with the wheel must not disturb the list's cursor or
// scroll position (regression: over-scrolling the diff jumped the list to the end).
func TestWheelScrollDoesNotLeakBetweenViews(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}

	// Park the list cursor at a non-zero row.
	m.cursor = 2
	m.clampCursor()
	savedCursor, savedTop := m.cursor, m.top

	// Enter the diff with scrollable content and scroll it hard past the bottom.
	m.state = stateDiff
	m.diffLines = make([]string, 200)
	m.diffTop = 0
	for range 500 {
		m.scroll(1)
	}
	if m.diffTop == 0 {
		t.Fatal("diff should have scrolled down")
	}
	if m.cursor != savedCursor || m.top != savedTop {
		t.Fatalf("diff scroll leaked into list: cursor %d->%d, top %d->%d",
			savedCursor, m.cursor, savedTop, m.top)
	}

	// Back in the list, the wheel drives the cursor (routed via Update/MouseMsg).
	m.state = stateList
	nm, _ := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	if got := nm.(model).cursor; got != savedCursor+1 {
		t.Fatalf("list wheel should move cursor to %d, got %d", savedCursor+1, got)
	}
}

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// truncate measures terminal cells, so multibyte text must never be sliced
// mid-rune (which emitted invalid UTF-8) and wide characters must not overflow.
func TestTruncateDisplayWidth(t *testing.T) {
	for _, w := range []int{1, 4, 9, 10, 11, 40} {
		for _, s := range []string{"日本語のコミットです", "feat: 🚀 ship it", "feature/café", "plain-ascii"} {
			got := truncate(s, w)
			if !utf8.ValidString(got) {
				t.Fatalf("truncate(%q, %d) = %q: invalid UTF-8", s, w, got)
			}
			if cells := ansi.StringWidth(got); cells > w {
				t.Fatalf("truncate(%q, %d) = %q: %d cells, over budget", s, w, got, cells)
			}
		}
	}
	if got := truncate("plain-ascii", 40); got != "plain-ascii" {
		t.Fatalf("short strings must pass through unchanged, got %q", got)
	}
}

// pad must align on display cells; wide glyphs otherwise push later columns.
func TestPadDisplayWidth(t *testing.T) {
	for _, s := range []string{"日本語", "ab", "🚀", ""} {
		if got := ansi.StringWidth(pad(s, 10)); got != 10 {
			t.Fatalf("pad(%q, 10) is %d cells, want 10", s, got)
		}
	}
}

// Every track value must occupy the same column width, or the columns after it
// shift on gone rows (regression: "gone" was 8 wide where others were 10).
func TestTrackColumnAlignment(t *testing.T) {
	m := model{width: 120}
	rows := []branch{
		{name: "aaa", upstream: "origin/aaa", ahead: 2, behind: 1},
		{name: "bbb", upstream: "origin/bbb", gone: true},
		{name: "ccc"},
		{name: "ddd", upstream: "origin/ddd", remoteMerged: true},
	}
	want := -1
	for _, br := range rows {
		m.branches = []branch{br}
		plain := stripANSI(m.renderRow(0, 10))
		at := strings.Index(plain, "0001-Jan-01") // the column right after track
		if at < 0 {
			t.Fatalf("date column missing for %q: %q", br.name, plain)
		}
		// Compare display cells, not the byte offset: the arrows in the track
		// column are multibyte, which is the whole point of this alignment.
		cells := ansi.StringWidth(plain[:at])
		if want == -1 {
			want = cells
		} else if cells != want {
			t.Fatalf("branch %q: date column at cell %d, want %d (track width differs)\n%q",
				br.name, cells, want, plain)
		}
	}
}

// The core safety fix: a gone branch holding commits that are not in the base
// is force-deleted with -D, so it must be measured, warned about on the confirm
// screen, and left out of `p`'s auto-selection.
func TestGoneBranchWithUnpushedCommitsIsGuarded(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	// feature/tracked is pushed; add a commit that never reaches the remote,
	// then delete the remote branch and prune so it goes "gone".
	git(t, repo, "checkout", "-q", "feature/tracked")
	commitFile(t, repo, "unpushed", "irreplaceable")
	git(t, repo, "checkout", "-q", "main")
	git(t, repo, "push", "-q", "origin", "--delete", "feature/tracked")
	git(t, repo, "fetch", "-q", "--all", "--prune")

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	tb := find(m.branches, "feature/tracked")
	if tb == nil || !tb.gone {
		t.Fatalf("feature/tracked should be gone: %+v", tb)
	}
	if tb.ahead != 0 {
		t.Fatalf("precondition: git reports no ahead count for gone branches, got %d", tb.ahead)
	}
	if tb.riskCommits == 0 {
		t.Fatal("gone branch with unpushed commits must report riskCommits > 0")
	}

	// The confirm screen must name the cost before anything is deleted.
	tb.selected = true
	m.state = stateConfirm
	out := stripANSI(m.confirmView())
	if !strings.Contains(out, "force delete (-D) will discard them") {
		t.Fatalf("confirm view must warn about discarded commits:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("%d commit(s) not in", tb.riskCommits)) {
		t.Fatalf("confirm view must state the commit count:\n%s", out)
	}

	// `p` must not auto-select it: discarding those commits stays deliberate.
	for i := range m.branches {
		m.branches[i].selected = false
	}
	nm, _ := m.Update(fetchDoneMsg{})
	after := nm.(model)
	if b := find(after.branches, "feature/tracked"); b == nil || b.selected {
		t.Fatalf("gone branch holding unique commits must not be auto-selected: %+v", b)
	}
	if !strings.Contains(after.status, "hold commits not in") {
		t.Fatalf("status should report the skipped branches, got %q", after.status)
	}
}

// A gone branch whose work is already in the base carries no risk, so `p` still
// auto-selects it — the headline prune workflow must stay one keystroke.
func TestGoneMergedBranchStillAutoSelected(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	// feature/tracked's tip is already an ancestor of main's content here: reset
	// it to main, push, then delete the remote branch and prune.
	git(t, repo, "branch", "-f", "feature/tracked", "main")
	git(t, repo, "push", "-qf", "origin", "feature/tracked")
	git(t, repo, "push", "-q", "origin", "--delete", "feature/tracked")
	git(t, repo, "fetch", "-q", "--all", "--prune")

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	b := find(m.branches, "feature/tracked")
	if b == nil || !b.gone || b.riskCommits != 0 {
		t.Fatalf("merged gone branch should carry no risk: %+v", b)
	}
	// The headline prune must stay warning-free: -D discards nothing here.
	if w := m.riskWarning(*b); w != "" {
		t.Fatalf("a gone branch holding nothing unique must not warn: %q", w)
	}
	nm, _ := m.Update(fetchDoneMsg{})
	after := nm.(model)
	if b := find(after.branches, "feature/tracked"); b == nil || !b.selected {
		t.Fatalf("risk-free gone branch should still be auto-selected: %+v", b)
	}
	if !strings.Contains(after.status, "press d to prune") {
		t.Fatalf("status should offer the prune, got %q", after.status)
	}
}

// riskCommitCount uses patch-equivalence, so a cherry-picked commit already in
// the base is not counted as work at risk.
func TestRiskCommitCountIgnoresCherryPicked(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	git(t, repo, "checkout", "-q", "-b", "picked", "main")
	commitFile(t, repo, "p", "p")
	git(t, repo, "checkout", "-q", "main")
	git(t, repo, "cherry-pick", "picked")

	if n := riskCommitCount("picked", "main"); n != 0 {
		t.Fatalf("cherry-picked commit should not count as at risk, got %d", n)
	}
	if n := riskCommitCount("feature/unmerged", "main"); n != 1 {
		t.Fatalf("genuinely unmerged commit should count, got %d", n)
	}
	// No base to compare against means nothing can be asserted about risk.
	if n := riskCommitCount("picked", ""); n != 0 {
		t.Fatalf("empty base should yield 0, got %d", n)
	}
}

// Merge info must resolve on repos whose only remote is not named "origin";
// previously remoteDefault() returned "" and the safety indicators vanished.
func TestNonOriginRemoteResolves(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	// Rename origin -> upstream, leaving no remote called "origin".
	git(t, repo, "remote", "rename", "origin", "upstream")
	git(t, repo, "fetch", "-q", "--all", "--prune")

	if got := remoteDefault(); got != "upstream/main" {
		t.Fatalf("remoteDefault should resolve upstream/main, got %q", got)
	}
	if got := baseBranch("feature/unmerged"); got != "upstream/main" {
		t.Fatalf("baseBranch should use the non-origin remote, got %q", got)
	}
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	if m.remoteDefault != "upstream/main" || m.riskBase != "upstream/main" {
		t.Fatalf("model should cache the resolved default: %q / %q", m.remoteDefault, m.riskBase)
	}
}

// origin is preferred when several remotes are configured.
func TestRemotesPrefersOrigin(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	git(t, repo, "remote", "add", "aaa-fork", repo)
	got := remotes()
	if len(got) == 0 || got[0] != "origin" {
		t.Fatalf("origin should sort first, got %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("want both remotes, got %v", got)
	}
}

// commitFile writes a file on the current branch and commits it.
func commitFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(dir+"/"+name, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", name)
	git(t, dir, "commit", "-qm", "add "+name)
}

// setupDeleteShapes adds one branch for every shape that decides whether
// `git branch -d` is accepted, so predictions can be checked against real git.
// setupRepo already supplies the two upstream-less shapes (feature/unmerged and
// feature/merged), so they are not rebuilt here.
func setupDeleteShapes(t *testing.T, repo string) {
	t.Helper()
	// Upstream, with a local commit the upstream lacks.
	git(t, repo, "checkout", "-q", "-b", "shape/ahead", "main")
	git(t, repo, "push", "-q", "-u", "origin", "shape/ahead")
	commitFile(t, repo, "ahead", "x")

	// Ahead of a live upstream, but merged into HEAD. git consults the upstream
	// alone whenever it resolves, so it refuses this even though HEAD holds the
	// work — the case an either-or reading of the two criteria gets wrong.
	git(t, repo, "checkout", "-q", "-b", "shape/ahead-head-merged", "main")
	git(t, repo, "push", "-q", "-u", "origin", "shape/ahead-head-merged")
	commitFile(t, repo, "ahead-head-merged", "x")
	git(t, repo, "checkout", "-q", "main")
	git(t, repo, "merge", "-q", "--no-ff", "-m", "merge shape/ahead-head-merged", "shape/ahead-head-merged")

	// Upstream, strictly behind it — an ancestor, so merged into its upstream.
	git(t, repo, "checkout", "-q", "-b", "shape/behind", "main")
	git(t, repo, "push", "-q", "-u", "origin", "shape/behind")
	commitFile(t, repo, "behind", "x")
	git(t, repo, "push", "-q", "origin", "shape/behind")
	git(t, repo, "reset", "-q", "--hard", "HEAD~1")

	// Squash-merged into origin/main: different commits, same content, upstream
	// still present.
	git(t, repo, "checkout", "-q", "-b", "shape/squashed", "main")
	commitFile(t, repo, "squashed", "x")
	git(t, repo, "push", "-q", "-u", "origin", "shape/squashed")
	git(t, repo, "checkout", "-q", "main")
	git(t, repo, "merge", "-q", "--squash", "shape/squashed")
	git(t, repo, "commit", "-qm", "squash shape/squashed")
	git(t, repo, "push", "-q", "origin", "main")

	git(t, repo, "checkout", "-q", "main")
	git(t, repo, "fetch", "-q", "--all", "--prune")
}

// Ground truth: safeDeletable must agree with what `git branch -d` actually does
// for every branch shape. This is the predicate the confirm-screen warning and
// the risk measurement both hang off, so a wrong answer here is either a silent
// delete failure (the reported bug) or a false alarm.
func TestSafeDeletableMatchesGit(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	setupDeleteShapes(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"shape/ahead":             false, // holds a commit its upstream lacks
		"shape/ahead-head-merged": false, // ditto, and HEAD holding it does not help
		"shape/behind":            true,  // ancestor of its upstream
		"shape/squashed":          true,  // equal to its upstream
		"feature/unmerged":        false, // no upstream, unmerged
		"feature/merged":          true,  // no upstream, merged into HEAD
	}
	for name, expect := range want {
		b := find(m.branches, name)
		if b == nil {
			t.Fatalf("%s missing from the branch list", name)
		}
		if b.safeDeletable() != expect {
			t.Errorf("%s: safeDeletable()=%v want %v (upstream=%q ahead=%d headMerged=%v)",
				name, b.safeDeletable(), expect, b.upstream, b.ahead, b.headMerged)
		}
		// Now ask git itself. Deleting one branch cannot change another's merge
		// status, so the whole set can be checked in one pass.
		_, gitErr := runGit("branch", "-d", name)
		if (gitErr == nil) != expect {
			t.Errorf("git branch -d %s: err=%v, want accepted=%v", name, gitErr, expect)
		}
		if gitErr != nil && !strings.Contains(gitErr.Error(), "not fully merged") {
			t.Errorf("%s refused for an unexpected reason: %v", name, gitErr)
		}
	}
}

// The reported bug: an unmerged branch with no upstream was deleted with -d and
// failed, with nothing on the confirm screen having warned about it.
func TestUnmergedBranchWithoutUpstreamIsWarned(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	b := find(m.branches, "feature/unmerged")
	if b == nil || b.upstream != "" || b.ahead != 0 {
		t.Fatalf("precondition: want an upstream-less branch with ahead=0: %+v", b)
	}
	if b.safeDeletable() {
		t.Fatalf("an unmerged upstream-less branch is not safely deletable: %+v", b)
	}

	// Drive the real path: 'd' measures the selection's risk, then confirms.
	b.selected = true
	nm, _ := m.updateList(key("d"))
	m = nm.(model)
	if m.state != stateConfirm {
		t.Fatalf("'d' should open the confirm screen, got %v", m.state)
	}
	b = find(m.branches, "feature/unmerged")
	if b.riskCommits != 1 {
		t.Fatalf("want the branch's 1 unique commit measured, got %d", b.riskCommits)
	}
	out := stripANSI(m.confirmView())
	if !strings.Contains(out, "safe delete (-d) will fail") {
		t.Fatalf("confirm view must warn that -d will be refused:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("%d commit(s) not in %s", b.riskCommits, m.riskBase)) {
		t.Fatalf("confirm view must state what a force would discard:\n%s", out)
	}
}

// Negative spec: branches git will happily delete must draw no warning at all,
// or the confirm screen cries wolf on the ordinary cleanup path.
func TestNoWarningForSafelyDeletableBranches(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	setupDeleteShapes(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"feature/merged", "shape/behind", "shape/squashed"} {
		b := find(m.branches, name)
		if b == nil {
			t.Fatalf("%s missing", name)
		}
		if w := m.riskWarning(*b); w != "" {
			t.Errorf("%s deletes cleanly but warned: %q", name, w)
		}
		b.selected = true
	}
	m.state = stateConfirm
	// Only the delete-risk lines are asserted on: the separate "not merged into
	// <default>" indicator is about the remote's state, not about whether the
	// delete will succeed, and legitimately fires for squash-merged branches.
	out := stripANSI(m.confirmView())
	for _, phrase := range []string{"safe delete (-d) will fail", "force delete (-D)", "not fully merged"} {
		if strings.Contains(out, phrase) {
			t.Fatalf("confirm view must not warn %q for clean branches:\n%s", phrase, out)
		}
	}
}

// The force prompt asks the user to approve a -D, so it must state what that
// costs. It used to print the ahead count, which is 0 for exactly the branches
// that reach the prompt without an upstream — leaving the prompt blank.
func TestForcePromptStatesCommitCount(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	find(m.branches, "feature/unmerged").selected = true
	m.force = false
	m.performDeletions()

	failures := m.forceableFailures()
	if len(failures) != 1 {
		t.Fatalf("want one refused delete, got %+v", failures)
	}
	if failures[0].br.riskCommits != 1 {
		t.Fatalf("the refused result must carry its measured cost, got %+v", failures[0])
	}

	m.state = stateForcePrompt
	out := stripANSI(m.forcePromptView())
	if !strings.Contains(out, fmt.Sprintf("1 commit(s) not in %s will be lost", m.riskBase)) {
		t.Fatalf("force prompt must state the commit count:\n%s", out)
	}
}

// The remote copy is the last place unmerged commits survive a refused local
// delete, so the armed push must be deferred — and then honoured once the force
// retry actually removes the branch.
func TestRemoteDeleteDeferredUntilLocalSucceeds(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	// Give feature/tracked a commit its upstream lacks, so -d is refused.
	git(t, repo, "checkout", "-q", "feature/tracked")
	commitFile(t, repo, "unpushed", "x")
	git(t, repo, "checkout", "-q", "main")

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	tb := find(m.branches, "feature/tracked")
	if tb == nil || tb.ahead != 1 {
		t.Fatalf("precondition: feature/tracked should be ahead 1: %+v", tb)
	}
	tb.selected = true
	tb.deleteRemote = true
	m.force = false // safe delete, which git will refuse
	m.performDeletions()

	r := m.results[0]
	if r.localOK {
		t.Fatalf("safe delete should have been refused: %+v", r)
	}
	if r.remoteTried {
		t.Fatalf("the remote must not be touched while the local branch survives: %+v", r)
	}
	if !r.remoteSkipped {
		t.Fatalf("the deferred push must be recorded: %+v", r)
	}
	if !remoteHasBranch(t, repo, "feature/tracked") {
		t.Fatal("remote feature/tracked must survive a refused local delete")
	}
	if out := stripANSI(m.resultView()); !strings.Contains(out, "kept remote origin/feature/tracked") {
		t.Fatalf("results must explain the kept remote:\n%s", out)
	}

	// The force retry clears the branch, so the arming is finally honoured.
	m.forceDeleteUnmerged()
	r = m.results[0]
	if !r.localOK || !r.remoteTried || !r.remoteOK || r.remoteSkipped {
		t.Fatalf("force retry should complete both deletes: %+v", r)
	}
	if remoteHasBranch(t, repo, "feature/tracked") {
		t.Fatal("remote feature/tracked should be deleted after the force retry")
	}
}

// Negative spec: a delete refused for any reason other than unmerged commits
// cannot be rescued by -D, so it must not raise the force prompt — which would
// both mislabel the cause and offer a retry that fails identically.
func TestNonUnmergedFailureIsNotForceable(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	// A second worktree holds feature/unmerged; git refuses to delete it under
	// -d and -D alike.
	wt := t.TempDir() + "/wt"
	git(t, repo, "worktree", "add", "-q", wt, "feature/unmerged")

	if _, err := runGit("branch", "-D", "feature/unmerged"); err == nil {
		t.Fatal("precondition: -D should also fail for a branch held by a worktree")
	}

	b := branch{name: "feature/unmerged"}
	res := deleteBranch(b, "-d", false)
	if res.localOK {
		t.Fatalf("delete should have failed: %+v", res)
	}
	if res.forceable {
		t.Fatalf("a worktree conflict must not be offered as force-retryable: %q", res.localErr)
	}

	// End to end: the async path lands on the results screen, not the prompt.
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	m.results = []deleteResult{{br: branch{name: "feature/unmerged"}}}
	m.state = stateDeleting
	msg := deleteBranchCmd(0, *find(m.branches, "feature/unmerged"), "-d", false)()
	nm, _ := m.Update(msg)
	if got := nm.(model).state; got != stateResult {
		t.Fatalf("state should be stateResult, got %v", got)
	}
}

// The version string is well-formed even when VCS build info is absent.
func TestVersionString(t *testing.T) {
	s := versionString()
	for _, want := range []string{"git_pruner", "commit:", "date:", "go:"} {
		if !strings.Contains(s, want) {
			t.Fatalf("version output missing %q:\n%s", want, s)
		}
	}
}
