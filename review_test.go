package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// A teammate pushes to the branch after our last fetch. The ✓ and the commit
// count on screen describe the old commit, so the delete must not go through.
func TestRemoteDeleteRefusesMovedBranch(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	url := git(t, repo, "remote", "get-url", "origin")
	mate := filepath.Join(t.TempDir(), "mate")
	git(t, filepath.Dir(mate), "clone", "-q", url, mate)
	git(t, mate, "config", "user.email", "m@m.m")
	git(t, mate, "config", "user.name", "m")
	git(t, mate, "checkout", "-q", "feature/tracked")
	commitFile(t, mate, "new", "teammate work")
	git(t, mate, "push", "-q", "origin", "feature/tracked")
	pushed := git(t, mate, "rev-parse", "HEAD")

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	tb := find(m.branches, "feature/tracked")
	tb.selected, tb.deleteRemote = true, true
	m = startAndDrain(t, m, true)

	r := m.results[0]
	if r.remoteOK || !strings.Contains(r.remoteErr, "moved since the last fetch") {
		t.Fatalf("the delete of a moved remote branch must be refused: %+v", r)
	}
	if got := git(t, url, "rev-parse", "refs/heads/feature/tracked"); got != pushed {
		t.Fatalf("the remote branch must keep the teammate's commit %s, got %s", pushed, got)
	}
}

// The local branch is behind its remote branch. The remote result and the
// restore command must name the remote's commit, not the local tip.
func TestRemoteRestoreNamesRemoteCommit(t *testing.T) {
	repo := setupRepo(t)
	setupDeleteShapes(t, repo)
	chdir(t, repo)
	remoteTip := git(t, repo, "rev-parse", "refs/remotes/origin/shape/behind")
	localTip := git(t, repo, "rev-parse", "refs/heads/shape/behind")
	if remoteTip == localTip {
		t.Fatal("precondition: the branch must be behind its upstream")
	}
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	b := find(m.branches, "shape/behind")
	b.selected, b.deleteRemote = true, true
	m = startAndDrain(t, m, true)

	summary := restoreSummary(m.results)
	if !strings.Contains(summary, "git push origin "+remoteTip+":refs/heads/shape/behind") {
		t.Fatalf("the push restore must name the remote's commit %s:\n%s", remoteTip, summary)
	}
	if !strings.Contains(summary, "git branch shape/behind "+localTip) {
		t.Fatalf("the local restore must name the local tip %s:\n%s", localTip, summary)
	}
	if o := stripANSI(m.resultView()); !strings.Contains(o, "deleted remote origin/shape/behind (was "+remoteTip[:7]+")") {
		t.Fatalf("the result must show the remote's commit:\n%s", o)
	}
}

// A fetch that lands on the confirm screen brings a list with no commit counts
// and selects more branches. The screen must not stay open over that.
func TestReloadClosesConfirmScreen(t *testing.T) {
	repo := setupRepo(t)
	git(t, repo, "checkout", "-q", "-b", "gone/clean", "main")
	git(t, repo, "push", "-q", "-u", "origin", "gone/clean")
	git(t, repo, "checkout", "-q", "main")
	chdir(t, repo)
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	m = press(t, m, key("f"))
	cursorTo(t, &m, "feature/unmerged")
	m = press(t, m, key(" "), key("d"))
	wantState(t, m, stateConfirm, "d opens the confirm screen")
	if !strings.Contains(stripANSI(m.View()), "will discard") {
		t.Fatal("precondition: the confirm screen warns about the unmerged branch")
	}

	git(t, repo, "push", "-q", "origin", "--delete", "gone/clean")
	git(t, repo, "fetch", "-q", "--prune")
	nm, cmd := m.Update(fetchDoneMsg{})
	m = nm.(model)
	wantState(t, m, stateList, "a reload must close the confirm screen")
	nm, _ = m.Update(cmd())
	m = nm.(model)
	wantState(t, m, stateList, "the merge queries must not reopen it")

	// d measures the new list, so the warning is back with the new selection.
	m = press(t, m, key("d"))
	wantState(t, m, stateConfirm, "d opens the confirm screen again")
	if o := stripANSI(m.View()); !strings.Contains(o, "will discard") || !strings.Contains(o, "gone/clean") {
		t.Fatalf("the new confirm screen must show the warning and the new branch:\n%s", o)
	}
}

// y on the force prompt can push, and a push can wait out netTimeout, so none
// of it may run inside Update.
func TestForceRetryRunsOffTheUpdateLoop(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	find(m.branches, "feature/unmerged").selected = true
	m = startAndDrain(t, m, false)
	wantState(t, m, stateForcePrompt, "a refused -d raises the force prompt")

	before := gitProcs.total.Load()
	nm, cmd := m.Update(key("y"))
	m = nm.(model)
	if n := gitProcs.total.Load() - before; n != 0 || cmd == nil {
		t.Fatalf("the force retry must run off the update loop: %d git processes ran inside Update", n)
	}
	wantState(t, m, stateDeleting, "the retry shows the deleting screen")
	if m.deletesDone() != 0 {
		t.Fatal("the retried result must read as in progress")
	}
	m = drainDeletions(t, m, cmd)
	wantState(t, m, stateResult, "the retry ends on the results screen")
	if r := m.results[0]; !r.localOK || r.forceable {
		t.Fatalf("the retry must delete the branch: %+v", r)
	}
}

// An undo answers with indexes into the results of the run it undoes. A new
// run started before it lands would replace that list under it.
func TestUndoHoldsNewDeleteRun(t *testing.T) {
	chdir(t, setupManyTracked(t, 3))
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	m.force = true
	find(m.branches, "feature/0").selected = true
	find(m.branches, "feature/1").selected = true
	m = startAndDrain(t, m, false)

	nm, undo := m.Update(key("u"))
	m = nm.(model)
	if undo == nil || !m.undoing {
		t.Fatal("u must start the restore")
	}
	find(m.branches, "feature/2").selected = true
	nm, cmd := m.Update(key("d"))
	m = nm.(model)
	if cmd != nil || m.state != stateList || !strings.Contains(m.status, "still restoring") {
		t.Fatalf("d must wait for the undo: state %d, status %q", m.state, m.status)
	}
	if nm, again := m.Update(key("u")); again != nil || !nm.(model).undoing {
		t.Fatal("a second u must not start a second restore")
	}

	nm, next := m.Update(undo())
	m = settle(m, nm.(model), next)
	if m.undoing || !m.results[0].restored || !m.results[1].restored {
		t.Fatalf("the undo must land on its own results: %+v", m.results)
	}
	find(m.branches, "feature/2").selected = true
	m = press(t, m, key("d"))
	wantState(t, m, stateConfirm, "d works again once the undo has landed")
}

// Before the merge queries land no branch reads as merged, so m would select
// nothing and say so as if it were the answer.
func TestMergedOldWaitsForMergeStatus(t *testing.T) {
	chdir(t, setupRepo(t))
	m, err := startupModel()
	if err != nil {
		t.Fatal(err)
	}
	nm, _ := m.Update(key("m"))
	m = nm.(model)
	if m.state != stateList || !strings.Contains(m.status, "still reading merge status") {
		t.Fatalf("m must wait for the merge queries: state %d, status %q", m.state, m.status)
	}
	nm, _ = m.Update(m.mergeCmd()())
	m = nm.(model)
	m = press(t, m, key("m"))
	wantState(t, m, stateAge, "m opens its prompt once the merge status is known")
}
