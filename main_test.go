package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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
	if err := os.WriteFile(tmp+"/a", []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, tmp, "add", "a")
	git(t, tmp, "commit", "-qm", "init commit")
	git(t, tmp, "remote", "add", "origin", remote)
	git(t, tmp, "push", "-q", "-u", "origin", "main")
	git(t, tmp, "branch", "feature/merged") // merged into main -> safe delete
	git(t, tmp, "checkout", "-q", "-b", "feature/unmerged")
	if err := os.WriteFile(tmp+"/b", []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, tmp, "add", "b")
	git(t, tmp, "commit", "-qm", "wip")
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
		switch m.results[i].name {
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
	if len(failures) != 1 || failures[0].name != "feature/unmerged" {
		t.Fatalf("want feature/unmerged in forceableFailures, got %+v", failures)
	}
	if failures[0].ahead != 0 {
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
		if r.name == "feature/unmerged" && (!r.localOK || r.localErr != "") {
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

// The version string is well-formed even when VCS build info is absent.
func TestVersionString(t *testing.T) {
	s := versionString()
	for _, want := range []string{"git_pruner", "commit:", "date:", "go:"} {
		if !strings.Contains(s, want) {
			t.Fatalf("version output missing %q:\n%s", want, s)
		}
	}
}
