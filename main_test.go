package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// key builds a rune KeyMsg (e.g. "y", "R") for driving update handlers in tests.
func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// deleteMsgs runs every cmd in a deletion batch concurrently — as the tea
// runtime does — and returns the branchDeletedMsgs they produce. The batch is
// one cmd per branch plus the spinner tick, which is dropped: it only animates,
// and waiting on its 120ms timer would slow every caller.
func deleteMsgs(t *testing.T, cmd tea.Cmd) []branchDeletedMsg {
	t.Helper()
	if cmd == nil {
		t.Fatal("want a deletion batch cmd, got nil")
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("want a tea.BatchMsg, got %T", msg)
	}
	msgs := make(chan tea.Msg, len(batch))
	for _, c := range batch {
		go func() { msgs <- c() }()
	}
	want := len(batch) - 1
	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()
	var out []branchDeletedMsg
	for len(out) < want {
		select {
		case msg := <-msgs:
			if dm, ok := msg.(branchDeletedMsg); ok {
				out = append(out, dm)
			}
		case <-timeout.C:
			t.Fatalf("timed out after %d of %d deletions reported", len(out), want)
		}
	}
	return out
}

// drainDeletions runs a deletion batch and feeds each result back through
// Update, so tests exercise the path users actually run: the completion logic
// (reload, then force-prompt vs result) decides the end state rather than the
// test asserting it into place.
func drainDeletions(t *testing.T, m model, cmd tea.Cmd) model {
	t.Helper()
	for _, dm := range deleteMsgs(t, cmd) {
		nm, _ := m.Update(dm)
		m = nm.(model)
	}
	return m
}

// startAndDrain deletes m's selected branches and drives the run to completion.
// Starting the batch inside the call keeps m's copy and startDeletions' writes
// to it from being operands of one expression, where Go does not define which
// happens first.
func startAndDrain(t *testing.T, m model, includeRemote bool) model {
	t.Helper()
	return drainDeletions(t, m, m.startDeletions(includeRemote))
}

// selectAll marks every branch but the current one, arming the remote delete too
// when arm is set.
func selectAll(m *model, arm bool) {
	for i := range m.branches {
		if m.branches[i].isCurrent {
			continue
		}
		m.branches[i].selected = true
		m.branches[i].deleteRemote = arm
	}
}

// wantState asserts the state the machine landed in, with why it had to.
func wantState(t *testing.T, m model, want viewState, why string) {
	t.Helper()
	if m.state != want {
		t.Fatalf("%s: state is %v, want %v", why, m.state, want)
	}
}

// remoteHasBranch reports whether origin still has the named branch.
func remoteHasBranch(t *testing.T, dir, name string) bool {
	t.Helper()
	return git(t, dir, "ls-remote", "--heads", "origin", name) != ""
}

// git runs a git command in dir, failing the test on error, and returns its
// trimmed output for the callers that read it.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// initRepo creates an empty repository on the named initial branch with a commit
// identity configured. Callers add the commits, remotes and branches they need.
func initRepo(t *testing.T, branch string) string {
	t.Helper()
	tmp := t.TempDir()
	git(t, tmp, "init", "-q", "-b", branch)
	git(t, tmp, "config", "user.email", "t@t.t")
	git(t, tmp, "config", "user.name", "t")
	return tmp
}

// addOrigin gives dir a bare origin remote and pushes trunk to it.
func addOrigin(t *testing.T, dir, trunk string) {
	t.Helper()
	remote := t.TempDir()
	git(t, remote, "init", "--bare", "-q", "-b", trunk)
	git(t, dir, "remote", "add", "origin", remote)
	git(t, dir, "push", "-q", "-u", "origin", trunk)
}

// setupLocalRepo builds the branch shapes with no remote at all — a scratch
// project, or one that has simply never been pushed.
func setupLocalRepo(t *testing.T) string {
	t.Helper()
	tmp := initRepo(t, "main")
	commitFile(t, tmp, "a", "a")
	git(t, tmp, "branch", "feature/merged") // merged into main -> safe delete
	git(t, tmp, "checkout", "-q", "-b", "feature/unmerged")
	commitFile(t, tmp, "b", "b")
	git(t, tmp, "checkout", "-q", "main")
	return tmp
}

// setupRepo is setupLocalRepo plus an origin and a branch tracking it.
func setupRepo(t *testing.T) string {
	t.Helper()
	tmp := setupLocalRepo(t)
	addOrigin(t, tmp, "main")
	git(t, tmp, "checkout", "-q", "-b", "feature/tracked", "feature/unmerged")
	git(t, tmp, "push", "-q", "-u", "origin", "feature/tracked")
	git(t, tmp, "checkout", "-q", "main")
	return tmp
}

// setupManyTracked builds a repo whose n feature branches each track origin and
// hold a commit of their own, so a prune of the lot fans out widely.
func setupManyTracked(t *testing.T, n int) string {
	t.Helper()
	tmp := initRepo(t, "main")
	commitFile(t, tmp, "a", "a")
	addOrigin(t, tmp, "main")
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("feature/%d", i)
		git(t, tmp, "checkout", "-q", "-b", name, "main")
		commitFile(t, tmp, fmt.Sprintf("f%d", i), name)
	}
	git(t, tmp, "checkout", "-q", "main")
	git(t, tmp, "push", "-q", "-u", "origin", "--all") // one connection, not n
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

func branchNames(bs []branch) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.name
	}
	return out
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
	got := branchNames(m.branches)
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
	m = startAndDrain(t, m, true)

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
	m = startAndDrain(t, m, true)

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
	m = startAndDrain(t, m, true)

	// The refused unmerged branch should be surfaced for a force prompt, with
	// its ahead count copied onto the result (0 here: it has no upstream).
	wantState(t, m, stateForcePrompt, "a refused -d must raise the force prompt")
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

	// Answering yes at the prompt retries with -D and clears the branch.
	nm, _ := m.Update(key("y"))
	m = nm.(model)
	wantState(t, m, stateResult, "answering the prompt must land on the results screen")
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
	m = startAndDrain(t, m, true)

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
	m = startAndDrain(t, m, true)

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
	m = startAndDrain(t, m, true)
	before := len(m.branches)
	wantState(t, m, stateResult, "a clean delete must skip the force prompt")

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
	m = startAndDrain(t, m, true)

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
	msg := deleteBranchCmd(2, branch{name: "feature/merged"}, false, false)()
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
	dm2 := deleteBranchCmd(0, tracked, true, true)().(branchDeletedMsg)
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

// runGit is where the cap lives, so callers cannot escape it however many pile
// in at once — and it still runs them in parallel rather than one at a time.
func TestGitCallsAreCapped(t *testing.T) {
	chdir(t, setupLocalRepo(t))

	const callers = 40
	gitProcs.peak.Store(0)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runGit("rev-parse", "--git-dir")
		}()
	}
	wg.Wait()

	peak := gitProcs.peak.Load()
	if peak > maxLocalGit {
		t.Fatalf("%d callers ran %d git processes at once, cap is %d", callers, peak, maxLocalGit)
	}
	if peak < 2 {
		t.Fatalf("the cap serialized everything: peak %d", peak)
	}
	if n := gitProcs.inFlight.Load(); n != 0 {
		t.Fatalf("every slot should be released, %d still held", n)
	}
}

// Pruning a wide selection must not open a connection per branch: remotes
// throttle or refuse a burst, and a failed push is a branch deleted locally
// whose only other copy is still out there.
func TestWideDeleteStaysWithinCaps(t *testing.T) {
	const n = 20
	repo := setupManyTracked(t, n)
	chdir(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	selectAll(&m, true)
	m.force = true

	gitProcs.peak.Store(0)
	netProcs.peak.Store(0)
	m = startAndDrain(t, m, true)

	// Literal bounds, not the constants under test: raising a cap must break this
	// test rather than move the goalposts with it.
	const fewConnections, fewProcesses = 4, 12
	if maxRemotePush > fewConnections {
		t.Fatalf("maxRemotePush is %d; a prune should never ask a remote for more than %d connections", maxRemotePush, fewConnections)
	}
	if got := netProcs.peak.Load(); got > fewConnections {
		t.Fatalf("%d branches opened %d simultaneous connections to the remote", n, got)
	}
	if got := gitProcs.peak.Load(); got > fewProcesses {
		t.Fatalf("%d branches ran %d git processes at once", n, got)
	}
	if netProcs.peak.Load() == 0 {
		t.Fatal("no pushes were observed; the test is not measuring the remote path")
	}
	// The cap must not cost any of them their deletion.
	for _, r := range m.results {
		if !r.localOK || !r.remoteOK {
			t.Fatalf("throttled delete did not complete: %+v", r)
		}
	}
	if len(m.branches) != 1 {
		t.Fatalf("only the current branch should remain, got %v", branchNames(m.branches))
	}
}

// Risk is measured concurrently, one goroutine per branch writing its own slice
// element. Each branch must end up with its own count, not a neighbour's.
func TestConcurrentRiskMeasurementKeepsCountsWithBranch(t *testing.T) {
	repo := initRepo(t, "main")
	chdir(t, repo)
	commitFile(t, repo, "a", "a")

	// feature/i carries i commits of its own. Branching off one chain costs n
	// commits for the n distinct counts; a fresh branch each time costs n(n+1)/2.
	const n = 12
	git(t, repo, "checkout", "-q", "-b", "chain", "main")
	for i := 1; i <= n; i++ {
		commitFile(t, repo, fmt.Sprintf("f%d", i), "x")
		git(t, repo, "branch", fmt.Sprintf("feature/%d", i))
	}
	git(t, repo, "checkout", "-q", "main")
	git(t, repo, "branch", "-D", "chain")

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	selectAll(&m, false)
	m.measureSelectedRisk()

	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("feature/%d", i)
		b := find(m.branches, name)
		if b == nil || !b.riskMeasured {
			t.Fatalf("%s was not measured: %+v", name, b)
		}
		if b.riskCommits != i {
			t.Fatalf("%s should hold %d commits, measured %d", name, i, b.riskCommits)
		}
	}
}

// The live path only leaves stateDeleting once every branch has reported, and
// the branch list is not reloaded before then — a mid-run reload would renumber
// the results the outstanding messages are still indexing into.
func TestDeletingWaitsForEveryResult(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	find(m.branches, "feature/merged").selected = true   // -d succeeds
	find(m.branches, "feature/unmerged").selected = true // -d is refused
	m.force = false

	msgs := deleteMsgs(t, m.startDeletions(false))
	if len(msgs) != 2 {
		t.Fatalf("want 2 results, got %d", len(msgs))
	}
	for i, dm := range msgs {
		nm, _ := m.Update(dm)
		m = nm.(model)
		if !m.results[dm.idx].done {
			t.Fatalf("result %d should be marked done: %+v", dm.idx, m.results[dm.idx])
		}
		if i == len(msgs)-1 {
			break
		}
		wantState(t, m, stateDeleting, "state must hold at stateDeleting until the last result")
		if find(m.branches, "feature/merged") == nil {
			t.Fatal("the branch list must not be reloaded mid-run")
		}
	}
	wantState(t, m, stateForcePrompt, "the refused -d should raise the force prompt")
	if find(m.branches, "feature/merged") != nil {
		t.Fatal("the final result should have reloaded the branch list")
	}
}

// A result carrying an index outside the current run must be dropped rather
// than panic: the async cmds outlive nothing here, but the bounds check is the
// only thing standing between a stale message and an out-of-range write.
func TestStrayDeleteResultIsIgnored(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	find(m.branches, "feature/merged").selected = true
	m.startDeletions(false)

	nm, _ := m.Update(branchDeletedMsg{idx: 7, res: deleteResult{done: true}})
	m = nm.(model)
	wantState(t, m, stateDeleting, "a stray result must not complete the run")
	if m.deletesDone() != 0 {
		t.Fatalf("a stray result must not be counted, got %d", m.deletesDone())
	}
}

// Declining the force prompt leaves the refused branch — and its commits — in
// place. This is the escape hatch the -d/-D split exists for.
func TestForcePromptDeclineKeepsBranch(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	find(m.branches, "feature/unmerged").selected = true
	m.force = false
	m = startAndDrain(t, m, true)
	wantState(t, m, stateForcePrompt, "a refused -d must raise the force prompt")

	nm, _ := m.Update(key("n"))
	m = nm.(model)
	wantState(t, m, stateResult, "declining must land on the results screen")
	if find(m.branches, "feature/unmerged") == nil {
		t.Fatal("declining the force prompt must keep the branch")
	}
	if len(m.forceableFailures()) != 1 {
		t.Fatalf("the refusal must still be reported: %+v", m.results)
	}
}

// While deletions are in flight the keyboard is inert except for ctrl+c, so a
// stray keystroke cannot dismiss a run whose results have not landed yet.
func TestDeletingIgnoresKeysExceptCtrlC(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	m.state = stateDeleting

	for _, k := range []tea.KeyMsg{key("q"), key("y"), key("d")} {
		nm, cmd := m.Update(k)
		if got := nm.(model).state; got != stateDeleting {
			t.Fatalf("%v must not change state, got %v", k, got)
		}
		if cmd != nil {
			t.Fatalf("%v must not issue a cmd", k)
		}
	}

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c must abort")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("ctrl+c should quit, got %T", cmd())
	}
}

// The spinner re-arms itself only while deletions are running; once the run has
// landed the tick must die out rather than loop forever.
func TestSpinnerTickStopsAfterDeleting(t *testing.T) {
	m := model{state: stateDeleting}

	nm, cmd := m.Update(spinnerTickMsg{})
	if nm.(model).spinnerFrame != 1 {
		t.Fatalf("frame should advance, got %d", nm.(model).spinnerFrame)
	}
	if cmd == nil {
		t.Fatal("the tick must re-arm while deleting")
	}

	m.state = stateResult
	if _, cmd := m.Update(spinnerTickMsg{}); cmd != nil {
		t.Fatal("the tick must not re-arm once the run has landed")
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
		wantState(t, nm.(model), stateDeleting, "state should be stateDeleting")
		m = drainDeletions(t, nm.(model), cmd)
		wantState(t, m, stateResult, "a clean -D run should land on the results screen")
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
		wantState(t, nm.(model), stateDeleting, "state should be stateDeleting")
		m = drainDeletions(t, nm.(model), cmd)
		wantState(t, m, stateResult, "a clean -D run should land on the results screen")
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
		plain := stripANSI(m.renderRow(br, 10, false))
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

	// The resolvers hand back qualified refs; the model shortens them for display.
	if got := remoteDefault(); got != "refs/remotes/upstream/main" {
		t.Fatalf("remoteDefault should resolve upstream/main, got %q", got)
	}
	if got := baseBranch("feature/unmerged"); got != "refs/remotes/upstream/main" {
		t.Fatalf("baseBranch should use the non-origin remote, got %q", got)
	}
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	if m.remoteDefault != "upstream/main" || m.riskBase != "upstream/main" {
		t.Fatalf("model should cache the resolved default: %q / %q", m.remoteDefault, m.riskBase)
	}
	if m.riskBaseRef != "refs/remotes/upstream/main" {
		t.Fatalf("model should cache the ref form too: %q", m.riskBaseRef)
	}
}

// origin is preferred when several remotes are configured. The names come out
// of refs/remotes now, so the second remote has to be fetched to count — one
// that has never been fetched holds no ref a default branch could resolve to.
func TestRemotesPrefersOrigin(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	git(t, repo, "remote", "add", "aaa-fork", repo)
	git(t, repo, "fetch", "-q", "aaa-fork")
	got := loadRemoteRefs().names
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
	wantState(t, m, stateConfirm, "'d' should open the confirm screen")
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
	m = startAndDrain(t, m, true)

	failures := m.forceableFailures()
	if len(failures) != 1 {
		t.Fatalf("want one refused delete, got %+v", failures)
	}
	if failures[0].br.riskCommits != 1 {
		t.Fatalf("the refused result must carry its measured cost, got %+v", failures[0])
	}

	wantState(t, m, stateForcePrompt, "a refused -d must raise the force prompt")
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
	m = startAndDrain(t, m, true)

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
	wantState(t, m, stateForcePrompt, "a refused -d must raise the force prompt")
	nm, _ := m.Update(key("y"))
	m = nm.(model)
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

	// A second worktree holds the merged branch and an untracked file, so a
	// safe delete must refuse to remove the worktree, and with it the branch.
	wt := t.TempDir() + "/wt"
	git(t, repo, "worktree", "add", "-q", wt, "feature/merged")
	if err := os.WriteFile(wt+"/scratch", []byte("unsaved work"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	b := *find(m.branches, "feature/merged")
	res := deleteBranch(b, false, false)
	if res.localOK {
		t.Fatalf("delete should have failed: %+v", res)
	}
	if res.forceable {
		t.Fatalf("a dirty worktree must not be offered as force-retryable: %q", res.localErr)
	}

	// End to end: the async path lands on the results screen, not the prompt.
	m.results = []deleteResult{{br: b}}
	m.state = stateDeleting
	nm, _ := m.Update(deleteBranchCmd(0, b, false, false)())
	if got := nm.(model).state; got != stateResult {
		t.Fatalf("state should be stateResult, got %v", got)
	}
}

// A tag sharing a branch's name shadows it: git resolves refs/tags/<name> before
// refs/heads/<name>, so any bare name handed to git measures the tag instead. On
// the risk path that reports a branch's unmerged commits as already safe —
// precisely when -D is about to discard them. Release branches tagged with their
// own name (v1.2, release-3) make this an everyday shape.
func TestTagShadowingBranchName(t *testing.T) {
	t.Run("branch_name", func(t *testing.T) {
		repo := setupRepo(t)
		chdir(t, repo)
		// Tag the base commit with the name of a branch that is one commit ahead.
		git(t, repo, "tag", "feature/unmerged", "main")

		if n := riskCommitCount("feature/unmerged", "origin/main"); n != 1 {
			t.Fatalf("want the branch's 1 unique commit measured, got %d (the tag was measured)", n)
		}
		diff, _, err := loadDiff("feature/unmerged")
		if err != nil {
			t.Fatalf("loadDiff: %v", err)
		}
		if !strings.Contains(diff, "+++ b/b") {
			t.Fatalf("diff must show the branch's content, not the tag's:\n%s", diff)
		}

		m, err := initialModel()
		if err != nil {
			t.Fatal(err)
		}
		// The branch must load under its plain name. %(refname:short) reports the
		// shortest *unambiguous* name, which becomes "heads/feature/unmerged" the
		// moment the tag exists — breaking every name-keyed lookup downstream.
		b := find(m.branches, "feature/unmerged")
		if b == nil {
			t.Fatalf("branch must load under its plain name; loaded %v", branchNames(m.branches))
		}

		// End to end: the confirm screen must still state what the delete costs.
		b.selected = true
		nm, _ := m.updateList(key("d"))
		m = nm.(model)
		out := stripANSI(m.confirmView())
		if !strings.Contains(out, fmt.Sprintf("1 commit(s) not in %s", m.riskBase)) {
			t.Fatalf("confirm view must state the shadowed branch's cost:\n%s", out)
		}

		// And the delete must land on the branch, leaving the tag alone.
		m.force = true
		m = startAndDrain(t, m, true)
		if !m.results[0].localOK {
			t.Fatalf("delete failed: %s", m.results[0].localErr)
		}
		if find(m.branches, "feature/unmerged") != nil {
			t.Fatal("the branch should be gone")
		}
		if _, err := runGit("rev-parse", "--verify", "--quiet", "refs/tags/feature/unmerged"); err != nil {
			t.Fatal("the tag must survive a branch delete")
		}
	})

	// A tag named main/master, with no branch of that name, must not be adopted as
	// the comparison base — `rev-parse --verify main` is satisfied by the tag. This
	// was the same bug on the no-remote path: measured against the tag, a branch
	// holding real work reported nothing at risk, so a -D drew no warning at all.
	t.Run("local_default_name", func(t *testing.T) {
		repo := initRepo(t, "trunk")
		chdir(t, repo)
		commitFile(t, repo, "a", "a")
		git(t, repo, "checkout", "-q", "-b", "feature", "trunk")
		commitFile(t, repo, "b", "b")
		// The tag carries the feature tip, so measuring against it reports nothing.
		git(t, repo, "tag", "main", "feature")
		git(t, repo, "checkout", "-q", "trunk")

		if got := localDefaultBranch("", gitHasBranch); got != "" {
			t.Fatalf("a tag must not pose as the local default branch, got %q", got)
		}
		m, err := initialModel()
		if err != nil {
			t.Fatal(err)
		}
		if m.riskBase != "" {
			t.Fatalf("no honest base exists here, got %q", m.riskBase)
		}
		b := find(m.branches, "feature")
		b.selected = true
		m.force = true
		m.measureSelectedRisk()
		if w := m.riskWarning(*b); !strings.Contains(w, "no base branch to compare against") {
			t.Fatalf("a -D with no measurable base must say so rather than stay silent: %q", w)
		}
	})

	// A remote carrying both a branch and a tag of one name rejects a bare refspec
	// as matching more than one ref, deleting nothing.
	t.Run("remote_delete_refspec", func(t *testing.T) {
		repo := setupRepo(t)
		chdir(t, repo)
		git(t, repo, "tag", "feature/tracked", "main")
		git(t, repo, "push", "-q", "origin", "refs/tags/feature/tracked")

		m, err := initialModel()
		if err != nil {
			t.Fatal(err)
		}
		tb := find(m.branches, "feature/tracked")
		tb.selected = true
		tb.deleteRemote = true
		m.force = true
		m = startAndDrain(t, m, true)

		r := m.results[0]
		if !r.localOK {
			t.Fatalf("local delete failed: %s", r.localErr)
		}
		if !r.remoteTried || !r.remoteOK {
			t.Fatalf("the remote branch delete must succeed: tried=%v err=%s", r.remoteTried, r.remoteErr)
		}
		if remoteHasBranch(t, repo, "feature/tracked") {
			t.Fatal("the remote branch should be gone")
		}
		// Only the branch was asked for; the tag must survive.
		out, err := runGit("ls-remote", "--tags", "origin", "feature/tracked")
		if err != nil || strings.TrimSpace(out) == "" {
			t.Fatalf("the remote tag must survive a branch delete (err=%v out=%q)", err, out)
		}
	})

	// The upstream is read as a short name too, so a tag named after a
	// remote-tracking ref disambiguates it to "remotes/origin/…" — which would
	// then be split into a bogus remote named "remotes".
	t.Run("upstream_name", func(t *testing.T) {
		repo := setupRepo(t)
		chdir(t, repo)
		git(t, repo, "tag", "origin/feature/tracked", "main")

		m, err := initialModel()
		if err != nil {
			t.Fatal(err)
		}
		b := find(m.branches, "feature/tracked")
		if b == nil {
			t.Fatalf("feature/tracked missing; loaded %v", branchNames(m.branches))
		}
		if b.upstream != "origin/feature/tracked" {
			t.Fatalf("upstream must load unqualified, got %q", b.upstream)
		}
		if b.remoteName() != "origin" || b.remoteBranch() != "feature/tracked" {
			t.Fatalf("a shadowed upstream must still split correctly: %s / %s",
				b.remoteName(), b.remoteBranch())
		}
	})

	// The base is resolved to a short name too, so a tag can shadow it the same way
	// — and a wrong base silently changes every branch's measured risk.
	t.Run("base_name", func(t *testing.T) {
		repo := setupRepo(t)
		chdir(t, repo)
		// A tag named after the remote-tracking base, pointing at the work that is
		// supposed to be measured as missing from it.
		git(t, repo, "tag", "origin/main", "feature/unmerged")

		m, err := initialModel()
		if err != nil {
			t.Fatal(err)
		}
		if m.riskBase != "origin/main" {
			t.Fatalf("precondition: base should be origin/main, got %q", m.riskBase)
		}
		if n := riskCommitCount("feature/unmerged", m.riskBaseRef); n != 1 {
			t.Fatalf("want 1 commit at risk, got %d (the tag was used as the base)", n)
		}
	})
}

// A fetch mutates nothing local, so pressing `p` must not silently discard the
// selections the user already made — they are the whole reason to press d next.
func TestFetchPreservesSelections(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	find(m.branches, "feature/unmerged").selected = true
	tb := find(m.branches, "feature/tracked")
	tb.selected = true
	tb.deleteRemote = true

	nm, _ := m.Update(fetchPruneCmd()())
	after := nm.(model)

	if b := find(after.branches, "feature/unmerged"); b == nil || !b.selected {
		t.Fatalf("a manual selection must survive a fetch: %+v", b)
	}
	if b := find(after.branches, "feature/tracked"); b == nil || !b.selected || !b.deleteRemote {
		t.Fatalf("an armed remote delete must survive a fetch: %+v", b)
	}
	if b := find(after.branches, "feature/merged"); b == nil || b.selected {
		t.Fatalf("an untouched branch must stay unselected: %+v", b)
	}
}

// The armed remote delete is dropped when the fetch reveals the upstream is
// already gone: the push it would run can only fail, and the results screen would
// report that failure as if the user had asked for something impossible.
func TestFetchDisarmsRemoteForGoneBranch(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	tb := find(m.branches, "feature/tracked")
	tb.selected = true
	tb.deleteRemote = true

	// Someone else deletes the remote branch before this fetch lands.
	git(t, repo, "push", "-q", "origin", "--delete", "feature/tracked")

	nm, _ := m.Update(fetchPruneCmd()())
	after := nm.(model)

	b := find(after.branches, "feature/tracked")
	if b == nil || !b.gone {
		t.Fatalf("precondition: feature/tracked should be gone after the fetch: %+v", b)
	}
	if !b.selected {
		t.Fatalf("the selection itself must survive: %+v", b)
	}
	if b.deleteRemote {
		t.Fatalf("the armed remote must be disarmed once the upstream is gone: %+v", b)
	}
}

// Everyday shape: the PR merged, the remote branch was deleted, and the user is
// still standing on that branch when they press `p`. The current branch cannot be
// deleted, so it must never be auto-selected — and must survive if it somehow is.
func TestGoneCurrentBranchIsNotPruned(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	git(t, repo, "checkout", "-q", "feature/tracked")
	git(t, repo, "push", "-q", "origin", "--delete", "feature/tracked")

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	nm, _ := m.Update(fetchPruneCmd()())
	m = nm.(model)

	b := find(m.branches, "feature/tracked")
	if b == nil || !b.gone || !b.isCurrent {
		t.Fatalf("precondition: want the gone current branch: %+v", b)
	}
	if b.selected {
		t.Fatal("the checked-out branch must never be auto-selected for deletion")
	}
	// It cannot be pruned, so the status must not count it as one waiting to be.
	if strings.Contains(m.status, "press d to prune") {
		t.Fatalf("status must not offer to prune the current branch: %q", m.status)
	}

	// Selected by hand, git refuses — a failure -D cannot rescue, so it must not
	// raise the force prompt offering a retry that fails identically.
	b.selected = true
	m = startAndDrain(t, m, true)
	if len(m.results) != 1 || m.results[0].localOK {
		t.Fatalf("deleting the checked-out branch must fail: %+v", m.results)
	}
	if m.results[0].forceable {
		t.Fatalf("a checked-out branch cannot be rescued by -D: %q", m.results[0].localErr)
	}
	if find(m.branches, "feature/tracked") == nil {
		t.Fatal("feature/tracked must survive the refused delete")
	}
}

// setupTrunkRepo builds a repo whose default branch is neither main nor master,
// with origin/HEAD unset — what `git remote add` + push produces, as opposed to a
// clone, which is the only thing that sets origin/HEAD.
func setupTrunkRepo(t *testing.T) string {
	t.Helper()
	tmp := initRepo(t, "trunk")
	commitFile(t, tmp, "a", "a")
	addOrigin(t, tmp, "trunk")
	git(t, tmp, "checkout", "-q", "-b", "feature/unmerged")
	commitFile(t, tmp, "b", "b")
	git(t, tmp, "checkout", "-q", "trunk")
	return tmp
}

// A repo with no remote configured: every remote-derived lookup must degrade to
// the local default branch rather than going blank and silencing the warnings.
func TestLocalOnlyRepo(t *testing.T) {
	repo := setupLocalRepo(t)
	chdir(t, repo)

	if got := loadRemoteRefs().names; len(got) != 0 {
		t.Fatalf("no remotes should be configured, got %v", got)
	}
	if got := remoteDefault(); got != "" {
		t.Fatalf("remoteDefault should be empty, got %q", got)
	}

	m, err := initialModel()
	if err != nil {
		t.Fatalf("initialModel must work without a remote: %v", err)
	}
	if m.riskBase != "main" || m.riskBaseRef != "refs/heads/main" {
		t.Fatalf("risk should fall back to the local default: %q / %q", m.riskBase, m.riskBaseRef)
	}
	if _, base, err := loadDiff("feature/unmerged"); err != nil || base != "main" {
		t.Fatalf("diff base should fall back to local main, got %q err=%v", base, err)
	}

	// The cost of a delete is still measured and still named.
	b := find(m.branches, "feature/unmerged")
	b.selected = true
	m.measureSelectedRisk()
	if b.riskCommits != 1 {
		t.Fatalf("want 1 commit at risk against local main, got %d", b.riskCommits)
	}
	if w := m.riskWarning(*b); !strings.Contains(w, "1 commit(s) not in main") {
		t.Fatalf("warning must name the local base: %q", w)
	}

	// `p` is a harmless no-op rather than an error.
	nm, _ := m.Update(fetchPruneCmd()())
	m = nm.(model)
	if m.err != "" {
		t.Fatalf("fetch must succeed with no remotes: %q", m.err)
	}
	if !strings.Contains(m.status, "no gone branches") {
		t.Fatalf("status should report nothing to prune, got %q", m.status)
	}

	// And an ordinary delete still works. feature/unmerged is still selected from
	// above (it survives the fetch), so clear it first.
	find(m.branches, "feature/unmerged").selected = false
	find(m.branches, "feature/merged").selected = true
	m = startAndDrain(t, m, true)
	if len(m.results) != 1 || !m.results[0].localOK {
		t.Fatalf("merged branch should delete cleanly: %+v", m.results)
	}
}

// `git init` with nothing committed: the tool must open on an empty list rather
// than refusing to start or panicking on the missing HEAD. (git itself fails
// `branch --merged HEAD` here, which mergedSet has to absorb.)
func TestUnbornHeadRepo(t *testing.T) {
	repo := initRepo(t, "main")
	chdir(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatalf("initialModel must succeed in a fresh repo: %v", err)
	}
	if len(m.branches) != 0 {
		t.Fatalf("a repo with no commits has no branches, got %v", branchNames(m.branches))
	}
	if m.riskBase != "" || m.riskBaseRef != "" {
		t.Fatalf("nothing can serve as a base: %q / %q", m.riskBase, m.riskBaseRef)
	}
	if m.cur() != nil {
		t.Fatal("there is no branch under the cursor")
	}
	if out := stripANSI(m.listView()); !strings.Contains(out, "no local branches found") {
		t.Fatalf("the empty list must say so:\n%s", out)
	}

	// Every key that acts on the cursor or the selection must be a no-op here.
	for _, k := range []string{"j", "k", "g", "G", " ", "r", "a", "n", "v", "d", "s", "o", "f"} {
		nm, _ := m.updateList(key(k))
		m = nm.(model)
		if m.state != stateList {
			t.Fatalf("%q left the list view (state %v) with no branches", k, m.state)
		}
	}
	if len(m.selectedBranches()) != 0 {
		t.Fatal("nothing can be selected")
	}

	// Including a fetch, which has no remote to talk to.
	nm, _ := m.Update(fetchPruneCmd()())
	if got := nm.(model).err; got != "" {
		t.Fatalf("fetch in a fresh repo should not error: %q", got)
	}
}

// Mid-bisect, mid-rebase, or on a checked-out tag, HEAD is on no branch at all.
// Nothing is current, so nothing carries the current-branch protection — and git
// will happily delete the branch HEAD is parked on.
func TestDetachedHead(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	git(t, repo, "checkout", "-q", "--detach", "main")

	m, err := initialModel()
	if err != nil {
		t.Fatalf("initialModel must work on a detached HEAD: %v", err)
	}
	for _, b := range m.branches {
		if b.isCurrent {
			t.Fatalf("no branch is current when HEAD is detached: %+v", b)
		}
	}
	// git prints a "(HEAD detached at …)" pseudo-entry in --merged output; it must
	// not be mistaken for a branch, and real branches must still be classified.
	if len(m.branches) != 4 {
		t.Fatalf("want the 4 real branches, got %v", branchNames(m.branches))
	}
	if b := find(m.branches, "main"); b == nil || !b.headMerged {
		t.Fatalf("main is merged into the detached HEAD: %+v", b)
	}
	if b := find(m.branches, "feature/unmerged"); b == nil || b.headMerged {
		t.Fatalf("feature/unmerged is not merged into the detached HEAD: %+v", b)
	}

	// With nothing current, `a` selects everything — there is no branch to spare.
	nm, _ := m.updateList(key("a"))
	m = nm.(model)
	if got := len(m.selectedBranches()); got != len(m.branches) {
		t.Fatalf("select-all should take all %d branches, got %d", len(m.branches), got)
	}

	// Deleting the branch HEAD is parked on is legal while detached, and safe:
	// the commits stay reachable from HEAD.
	nm, _ = m.updateList(key("n"))
	m = nm.(model)
	find(m.branches, "main").selected = true
	m = startAndDrain(t, m, true)
	if len(m.results) != 1 || !m.results[0].localOK {
		t.Fatalf("main should delete cleanly while detached: %+v", m.results)
	}
	if find(m.branches, "main") != nil {
		t.Fatal("main should be gone")
	}
}

// A repo whose default branch is neither main nor master. Without origin/HEAD
// there is nothing left to guess from, so no base resolves at all — the path
// every "no base branch to compare against" message hangs off.
func TestNonStandardDefaultBranch(t *testing.T) {
	t.Run("unresolvable_base", func(t *testing.T) {
		repo := setupTrunkRepo(t)
		chdir(t, repo)

		if got := remoteDefault(); got != "" {
			t.Fatalf("nothing should resolve without origin/HEAD or main/master, got %q", got)
		}
		m, err := initialModel()
		if err != nil {
			t.Fatal(err)
		}
		if m.riskBase != "" {
			t.Fatalf("no base should resolve, got %q", m.riskBase)
		}

		b := find(m.branches, "feature/unmerged")
		if b == nil {
			t.Fatalf("feature/unmerged missing; loaded %v", branchNames(m.branches))
		}
		b.selected = true
		m.force = true
		m.measureSelectedRisk()
		if b.riskCommits != 0 {
			t.Fatalf("nothing can be measured without a base, got %d", b.riskCommits)
		}

		// A -D with nothing to measure against must say so rather than imply safety.
		m.state = stateConfirm
		if out := stripANSI(m.confirmView()); !strings.Contains(out, "no base branch to compare against") {
			t.Fatalf("confirm view must carry the warning:\n%s", out)
		}

		// The same holds on the force prompt reached after a refused safe delete.
		m.force = false
		m = startAndDrain(t, m, true)
		if len(m.forceableFailures()) != 1 {
			t.Fatalf("the unmerged branch should be refused and forceable: %+v", m.results)
		}
		wantState(t, m, stateForcePrompt, "a refused -d must raise the force prompt")
		if out := stripANSI(m.forcePromptView()); !strings.Contains(out, "no base branch to compare against") {
			t.Fatalf("force prompt must carry the warning:\n%s", out)
		}
	})

	// A clone sets origin/HEAD, which is the only thing that can name a default
	// branch the tool would never guess. This is remoteDefault's symbolic-ref path.
	t.Run("origin_head_resolves", func(t *testing.T) {
		repo := setupTrunkRepo(t)
		chdir(t, repo)
		git(t, repo, "remote", "set-head", "origin", "trunk")

		if got := remoteDefault(); got != "refs/remotes/origin/trunk" {
			t.Fatalf("origin/HEAD should name the default, got %q", got)
		}
		m, err := initialModel()
		if err != nil {
			t.Fatal(err)
		}
		if m.riskBase != "origin/trunk" || m.riskBaseRef != "refs/remotes/origin/trunk" {
			t.Fatalf("model should cache both forms: %q / %q", m.riskBase, m.riskBaseRef)
		}
		b := find(m.branches, "feature/unmerged")
		b.selected = true
		m.measureSelectedRisk()
		if b.riskCommits != 1 {
			t.Fatalf("risk should now be measurable against origin/trunk, got %d", b.riskCommits)
		}
	})
}

// The remote is still configured but unreachable — the server moved, the repo was
// deleted, or the laptop is offline. Remote-tracking refs are local, so everything
// on screen still resolves; only the operations that touch the network fail, and
// they must fail loudly without taking the local delete down with them.
func TestUnreachableRemote(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	git(t, repo, "remote", "set-url", "origin", t.TempDir()+"/gone.git")

	m, err := initialModel()
	if err != nil {
		t.Fatalf("initialModel must work against a dead remote: %v", err)
	}
	tb := find(m.branches, "feature/tracked")
	if tb == nil || tb.upstream != "origin/feature/tracked" {
		t.Fatalf("the upstream is recorded locally and must survive: %+v", tb)
	}

	// A fetch is the first thing to fail, and the error must reach the user.
	nm, _ := m.Update(fetchPruneCmd()())
	m = nm.(model)
	if m.err == "" {
		t.Fatal("a failed fetch must be reported")
	}
	if m.fetching {
		t.Fatal("the fetch flag must clear so p can be pressed again")
	}

	// The local delete still succeeds; the armed push fails and is reported.
	tb = find(m.branches, "feature/tracked")
	tb.selected = true
	tb.deleteRemote = true
	m = startAndDrain(t, m, true)

	r := m.results[0]
	if !r.localOK {
		t.Fatalf("the local delete must still land: %s", r.localErr)
	}
	if !r.remoteTried || r.remoteOK || r.remoteErr == "" {
		t.Fatalf("the push must have been tried, failed, and captured: %+v", r)
	}
	if r.remoteSkipped {
		t.Fatalf("the push was attempted, not deferred: %+v", r)
	}
	out := stripANSI(m.resultView())
	if !strings.Contains(out, "deleted local feature/tracked") {
		t.Fatalf("results must report the successful local delete:\n%s", out)
	}
	if !strings.Contains(out, "remote feature/tracked:") {
		t.Fatalf("results must report the failed push:\n%s", out)
	}
}

// setupDiverged gives feature/tracked commits its upstream lacks and the upstream
// a commit it lacks, so git reports "[ahead 2, behind 1]" — ahead by the "b" it
// already carried from setupRepo plus "mine", behind by "theirs".
func setupDiverged(t *testing.T, repo string) {
	t.Helper()
	git(t, repo, "checkout", "-q", "feature/tracked")
	commitFile(t, repo, "mine", "mine")
	// Publish a different commit as the upstream's tip, from a branch that never
	// touches the local one.
	git(t, repo, "checkout", "-q", "-b", "theirs", "main")
	commitFile(t, repo, "theirs", "theirs")
	git(t, repo, "push", "-qf", "origin", "theirs:feature/tracked")
	git(t, repo, "checkout", "-q", "main")
	git(t, repo, "branch", "-qD", "theirs")
	git(t, repo, "fetch", "-q", "--all", "--prune")
}

// Both halves of the track string come from one git field, but only the ahead
// half has ever been exercised against real git output.
func TestDivergedBranch(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	setupDiverged(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	b := find(m.branches, "feature/tracked")
	if b == nil || b.ahead != 2 || b.behind != 1 {
		t.Fatalf("want ahead=2 behind=1 parsed from real git: %+v", b)
	}
	if track := stripANSI(m.trackStr(*b)); !strings.Contains(track, "↑2") || !strings.Contains(track, "↓1") {
		t.Fatalf("the track column must show both directions, got %q", track)
	}
	// Holding a commit its upstream lacks, it is not safely deletable.
	if b.safeDeletable() {
		t.Fatalf("a branch ahead of its upstream must not be safely deletable: %+v", b)
	}

	// sortAheadBehind orders on (ahead - behind), which nets to 0 here.
	m.field = sortAheadBehind
	m.ascending = true
	m.sortBranches()
	if len(m.branches) != 4 {
		t.Fatalf("the sort must keep every branch: %v", branchNames(m.branches))
	}
}

// Someone else deleted the remote branch between the last fetch and this delete.
// Nothing local says so, so the push is still attempted — and because the refspec
// is qualified, git treats deleting an already-absent ref as a no-op rather than
// an error. The end state is exactly what the user armed, so this reports success.
// (A bare refspec would fail here with "remote ref does not exist", but it also
// silently deletes nothing when a tag shadows the branch — see
// TestTagShadowingBranchName/remote_delete_refspec. Idempotence is the better
// trade.)
func TestRemoteDeleteRace(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	// Delete it straight on the bare remote, leaving our remote-tracking ref stale
	// — `push --delete` from here would prune that ref and give the game away.
	out, err := runGit("remote", "get-url", "origin")
	if err != nil {
		t.Fatal(err)
	}
	git(t, strings.TrimSpace(out), "branch", "-qD", "feature/tracked")

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	tb := find(m.branches, "feature/tracked")
	if tb == nil || tb.gone || tb.upstream != "origin/feature/tracked" {
		t.Fatalf("without a fetch the branch still looks healthy: %+v", tb)
	}
	tb.selected = true
	tb.deleteRemote = true
	m = startAndDrain(t, m, true)

	r := m.results[0]
	if !r.localOK {
		t.Fatalf("the local delete must still succeed: %s", r.localErr)
	}
	if !r.remoteTried || !r.remoteOK {
		t.Fatalf("the already-absent remote must resolve cleanly: %+v", r)
	}
	if remoteHasBranch(t, repo, "feature/tracked") {
		t.Fatal("the remote branch must be absent afterwards")
	}
	if o := stripANSI(m.resultView()); !strings.Contains(o, "deleted remote feature/tracked") {
		t.Fatalf("results must report the remote as dealt with:\n%s", o)
	}
}

// The checked-out branch cannot be deleted, so no key may ever mark it.
func TestCurrentBranchCannotBeMarked(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	for i := range m.branches {
		if m.branches[i].isCurrent {
			m.cursor = i
		}
	}
	if m.cur() == nil || !m.cur().isCurrent {
		t.Fatal("precondition: the cursor should sit on the current branch")
	}
	for _, k := range []string{" ", "r"} {
		nm, _ := m.updateList(key(k))
		m = nm.(model)
		if b := m.cur(); b.selected || b.deleteRemote {
			t.Fatalf("%q marked the current branch: %+v", k, b)
		}
	}

	// Select-all spares it too.
	nm, _ := m.updateList(key("a"))
	m = nm.(model)
	if got, want := len(m.selectedBranches()), len(m.branches)-1; got != want {
		t.Fatalf("select-all should take %d of %d branches, got %d", want, len(m.branches), got)
	}
	for _, b := range m.selectedBranches() {
		if b.isCurrent {
			t.Fatalf("select-all must spare the current branch: %+v", b)
		}
	}
}

// With no remote default and no local main/master, there is nothing left to diff
// against but HEAD.
func TestLoadDiffFallsBackToHead(t *testing.T) {
	repo := setupTrunkRepo(t) // default branch "trunk", origin/HEAD unset
	chdir(t, repo)

	if got := baseBranch("feature/unmerged"); got != "" {
		t.Fatalf("no base should resolve, got %q", got)
	}
	diff, base, err := loadDiff("feature/unmerged")
	if err != nil {
		t.Fatalf("loadDiff: %v", err)
	}
	if base != "HEAD" {
		t.Fatalf("the base should fall back to HEAD, got %q", base)
	}
	if !strings.Contains(diff, "+++ b/b") {
		t.Fatalf("the diff against HEAD must still show the branch's work:\n%s", diff)
	}

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	m.diffBranch, m.diffBase = "feature/unmerged", base
	m.diffLines = strings.Split(strings.TrimRight(diff, "\n"), "\n")
	m.state = stateDiff
	if o := stripANSI(m.diffView()); !strings.Contains(o, "vs HEAD") {
		t.Fatalf("the diff header must name the base it actually used:\n%s", o)
	}
}

// Legal but unusual content has to survive the NUL-delimited parse: an empty
// commit subject leaves a trailing empty field, which a stricter field count
// would drop the whole branch over, and a multibyte name must reach the renderer
// and the git call intact.
func TestUnicodeNameAndEmptySubject(t *testing.T) {
	repo := setupLocalRepo(t)
	chdir(t, repo)

	const name = "feature/café-日本語"
	git(t, repo, "checkout", "-q", "-b", name)
	if err := os.WriteFile(repo+"/c", []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "c")
	git(t, repo, "commit", "-q", "--allow-empty-message", "-m", "")
	git(t, repo, "checkout", "-q", "main")

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	b := find(m.branches, name)
	if b == nil {
		t.Fatalf("the branch must survive the parse; loaded %v", branchNames(m.branches))
	}
	if b.subject != "" {
		t.Fatalf("want an empty subject, got %q", b.subject)
	}
	if !strings.Contains(stripANSI(m.listView()), "café") {
		t.Fatalf("the row must carry the branch name:\n%s", stripANSI(m.listView()))
	}

	// And it deletes end to end, the name surviving the round trip to git.
	b.selected = true
	m.force = true
	m = startAndDrain(t, m, true)
	if !m.results[0].localOK {
		t.Fatalf("delete failed: %s", m.results[0].localErr)
	}
	if find(m.branches, name) != nil {
		t.Fatal("the branch should be gone")
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

func TestHelpViewIncludesBuildMetadata(t *testing.T) {
	m := model{width: 80, height: 24}
	v := m.helpView()
	for _, want := range []string{"git_pruner — help", "build date:", "commit:"} {
		if !strings.Contains(v, want) {
			t.Fatalf("help view missing %q:\n%s", want, v)
		}
	}
}

func TestBuildInfoLdflagsOverride(t *testing.T) {
	origCommit, origDate, origTime := gitCommit, buildDate, buildTime
	defer func() {
		gitCommit, buildDate, buildTime = origCommit, origDate, origTime
	}()

	gitCommit = "abcdef123456"
	buildDate = "2026-09-01"
	buildTime = ""

	commit, date := buildInfo()
	if commit != "abcdef1" {
		t.Fatalf("expected truncated commit abcdef1, got %q", commit)
	}
	if date != "2026-09-01" {
		t.Fatalf("expected date 2026-09-01, got %q", date)
	}
}

// setupGoneMerged builds n branches that are merged into main and report their
// upstream as gone. The upstream is configured rather than pushed and pruned:
// git reads "gone" straight out of the config when the remote-tracking ref is
// absent, which keeps the fixture free of network work.
func setupGoneMerged(t *testing.T, n int) string {
	t.Helper()
	tmp := initRepo(t, "main")
	commitFile(t, tmp, "a", "a")
	git(t, tmp, "remote", "add", "origin", filepath.Join(t.TempDir(), "absent.git"))
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("feature/%d", i)
		git(t, tmp, "checkout", "-q", "-b", name, "main")
		commitFile(t, tmp, fmt.Sprintf("f%d", i), name)
		git(t, tmp, "checkout", "-q", "main")
		git(t, tmp, "merge", "-q", "--no-ff", name, "-m", "m"+name)
		git(t, tmp, "config", "branch."+name+".remote", "origin")
		git(t, tmp, "config", "branch."+name+".merge", "refs/heads/"+name)
	}
	return tmp
}

// A branch whose tip is already in the base has an empty base..branch range, so
// `git cherry` can only report nothing. One `branch --merged` query answers that
// for the whole list, and it has to stay one query: a subprocess per branch put
// half a second on the load of every repo full of pruned branches.
func TestMergedBranchesCostNoSubprocess(t *testing.T) {
	const n = 30
	chdir(t, setupGoneMerged(t, n))

	before := gitProcs.total.Load()
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	spawned := gitProcs.total.Load() - before

	// A literal bound, not a formula over n: a fan-out that comes back must fail
	// this rather than scale along with it.
	const fewProcesses = 12
	if spawned > fewProcesses {
		t.Fatalf("loading %d merged branches ran %d git processes; it should not grow with the branch count", n, spawned)
	}

	measured := 0
	for _, b := range m.branches {
		if !b.gone {
			continue
		}
		if !b.riskMeasured {
			t.Fatalf("%s was never measured, so p cannot tell whether it is safe to select", b.name)
		}
		if b.riskCommits != 0 {
			t.Fatalf("%s is merged into the base; the shortcut must agree with git cherry, got %d", b.name, b.riskCommits)
		}
		measured++
	}
	if measured != n {
		t.Fatalf("expected %d gone branches, saw %d", n, measured)
	}
}

// The shortcut must not swallow the branches it cannot answer for: one that is
// merged and one that is not have to come back with the counts git cherry gives.
func TestUnmergedBranchIsStillMeasured(t *testing.T) {
	repo := setupGoneMerged(t, 1) // feature/0: merged, gone
	chdir(t, repo)

	git(t, repo, "checkout", "-q", "-b", "feature/kept", "main")
	commitFile(t, repo, "kept", "kept")
	git(t, repo, "checkout", "-q", "main")
	git(t, repo, "config", "branch.feature/kept.remote", "origin")
	git(t, repo, "config", "branch.feature/kept.merge", "refs/heads/feature/kept")

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	if b := find(m.branches, "feature/0"); b == nil || b.riskCommits != 0 {
		t.Fatalf("the merged branch should cost nothing: %+v", b)
	}
	kept := find(m.branches, "feature/kept")
	if kept == nil || !kept.riskMeasured || kept.riskCommits != 1 {
		t.Fatalf("the unmerged branch still needs its real count: %+v", kept)
	}
}

// screenRows counts the rows a rendered view occupies.
func screenRows(view string) int { return len(strings.Split(strings.TrimRight(view, "\n"), "\n")) }

// The confirm screen asks a question whose answer deletes branches. A wide
// selection used to render every branch in full, pushing that question past the
// last row of the terminal — the user answered a prompt they could not read.
func TestConfirmPromptStaysOnScreen(t *testing.T) {
	const n = 30
	chdir(t, setupGoneMerged(t, n))

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	selectAll(&m, false)
	next, _ := m.Update(key("d"))
	m = next.(model)
	wantState(t, m, stateConfirm, "d with a selection opens the confirm screen")

	view := m.confirmView()
	if rows := screenRows(view); rows > m.height {
		t.Fatalf("%d selected branches rendered %d rows into a %d-row terminal", n, rows, m.height)
	}
	if !strings.Contains(stripANSI(view), "Delete these branches?") {
		t.Fatalf("the prompt must be on the screen:\n%s", stripANSI(view))
	}
}

// Windowing must not hide a branch from the user: every one of them has to be
// reachable by scrolling, and the prompt has to stay put while they do it.
func TestConfirmScrollReachesTheLastBranch(t *testing.T) {
	const n = 30
	chdir(t, setupGoneMerged(t, n))

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	selectAll(&m, false)
	m.state = stateConfirm

	sel := m.selectedBranches()
	last := sel[len(sel)-1].name // the confirm list follows the sort order
	if strings.Contains(stripANSI(m.confirmView()), last) {
		t.Fatalf("%s should start below the window; the fixture proves nothing", last)
	}

	next, _ := m.Update(key("G"))
	m = next.(model)
	view := stripANSI(m.confirmView())
	if !strings.Contains(view, last) {
		t.Fatalf("G must reach the last branch:\n%s", view)
	}
	if !strings.Contains(view, "Delete these branches?") {
		t.Fatalf("the prompt must survive scrolling:\n%s", view)
	}
	if rows := screenRows(m.confirmView()); rows > m.height {
		t.Fatalf("the scrolled screen is %d rows in a %d-row terminal", rows, m.height)
	}

	// The answer keys still answer; they are not swallowed by the scroll handler.
	next, _ = m.Update(key("n"))
	wantState(t, next.(model), stateList, "n cancels from a scrolled confirm screen")
}

// The results screen has the same shape and the same failure: a wide prune used
// to bury "press q/enter to quit" under its own output.
func TestResultViewWindowsItsBody(t *testing.T) {
	m := model{height: 24, width: 100, state: stateResult}
	for i := 0; i < 40; i++ {
		m.results = append(m.results, deleteResult{
			br: branch{name: "feature/" + strconv.Itoa(i)}, done: true, localOK: true,
		})
	}
	if rows := screenRows(m.resultView()); rows > m.height {
		t.Fatalf("40 results rendered %d rows into a %d-row terminal", rows, m.height)
	}
	if !strings.Contains(stripANSI(m.resultView()), "press q/enter to quit") {
		t.Fatal("the footer must stay on the screen")
	}

	next, _ := m.Update(key("G"))
	m = next.(model)
	if o := stripANSI(m.resultView()); !strings.Contains(o, "feature/39") {
		t.Fatalf("G must reach the last result:\n%s", o)
	}
}

// A body that fits needs no position line and no scrolling — the common case
// must not grow furniture it does not need.
func TestShortBodyRendersWhole(t *testing.T) {
	m := model{height: 24, width: 100, state: stateResult}
	for i := 0; i < 3; i++ {
		m.results = append(m.results, deleteResult{
			br: branch{name: "feature/" + strconv.Itoa(i)}, done: true, localOK: true,
		})
	}
	out := stripANSI(m.resultView())
	if strings.Contains(out, "scroll") {
		t.Fatalf("a body that fits should show no position line:\n%s", out)
	}
	for i := 0; i < 3; i++ {
		if !strings.Contains(out, "feature/"+strconv.Itoa(i)) {
			t.Fatalf("every result must be shown:\n%s", out)
		}
	}
}

// ---- branch switching (b) and name filtering (/) ----

// special builds a non-rune KeyMsg (esc, enter, backspace) for the handlers.
func special(k tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: k} }

// press feeds keys through Update — the state machine's own dispatch — and
// returns the resulting model. Any cmds the keys produce are dropped.
func press(t *testing.T, m model, keys ...tea.KeyMsg) model {
	t.Helper()
	for _, k := range keys {
		nm, _ := m.Update(k)
		m = nm.(model)
	}
	return m
}

// cursorTo puts the cursor on the named branch's view row, failing if the
// branch is not visible.
func cursorTo(t *testing.T, m *model, name string) {
	t.Helper()
	m.focusBranch(name)
	if b := m.cur(); b == nil || b.name != name {
		t.Fatalf("branch %q not visible", name)
	}
}

// doSwitch presses c and, when a switch starts, drives its async cmd to
// completion the way the tea runtime would.
func doSwitch(t *testing.T, m model) model {
	t.Helper()
	nm, cmd := m.Update(key("c"))
	m = nm.(model)
	if cmd == nil {
		return m // guarded no-op: nothing was started
	}
	nm, _ = m.Update(cmd())
	return nm.(model)
}

// currentBranch reports the checked-out branch of dir.
func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	return git(t, dir, "symbolic-ref", "--short", "HEAD")
}

func TestSwitchBranch(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}

	// Marks must survive the switch — except on the branch that becomes
	// current, which the list refuses to operate on.
	find(m.branches, "feature/tracked").selected = true
	find(m.branches, "feature/merged").selected = true

	cursorTo(t, &m, "feature/merged")
	m = doSwitch(t, m)

	if got := currentBranch(t, repo); got != "feature/merged" {
		t.Fatalf("current branch is %q, want feature/merged", got)
	}
	if b := find(m.branches, "feature/merged"); !b.isCurrent || b.selected {
		t.Fatalf("switched-to branch must be current and unmarked: %+v", b)
	}
	if !find(m.branches, "feature/tracked").selected {
		t.Fatal("marks on other branches must survive the switch")
	}
	if b := m.cur(); b == nil || b.name != "feature/merged" {
		t.Fatalf("cursor must follow the switched branch, is on %+v", b)
	}
	if m.status == "" || m.err != "" {
		t.Fatalf("want a status and no error, got status=%q err=%q", m.status, m.err)
	}
}

func TestSwitchBranchRefusedKeepsState(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}

	// feature/unmerged committed a file named b; an untracked b on main makes
	// git refuse the switch rather than overwrite it.
	if err := os.WriteFile(filepath.Join(repo, "b"), []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	cursorTo(t, &m, "feature/unmerged")
	m = doSwitch(t, m)

	if got := currentBranch(t, repo); got != "main" {
		t.Fatalf("a refused switch must keep main current, got %q", got)
	}
	if m.err == "" {
		t.Fatal("a refused switch must surface git's reason")
	}
}

func TestSwitchOnCurrentBranchIsNoop(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	cursorTo(t, &m, "main")
	m = doSwitch(t, m)
	if m.err != "" || m.status != "" {
		t.Fatalf("b on the current branch must do nothing, got status=%q err=%q", m.status, m.err)
	}
	if got := currentBranch(t, repo); got != "main" {
		t.Fatalf("current branch moved to %q", got)
	}
}

func TestFilterNarrowsAndClears(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}

	m = press(t, m, key("/"))
	if m.state != stateFilter {
		t.Fatal("/ must start filter input")
	}
	m = press(t, m, key("feat"))
	if got := len(m.viewIdx()); got != 3 {
		t.Fatalf("filter feat: %d rows visible, want 3", got)
	}
	if out := stripANSI(m.listView()); !strings.Contains(out, "filter: feat") {
		t.Fatalf("header must show the filter:\n%s", out)
	}

	// While typing, action keys are text: q filters, it must not quit.
	nm, cmd := m.Update(key("q"))
	m = nm.(model)
	if cmd != nil || m.filter != "featq" {
		t.Fatalf("q while typing must extend the filter, got %q", m.filter)
	}
	m = press(t, m, special(tea.KeyBackspace))
	if m.filter != "feat" {
		t.Fatalf("backspace must trim the filter, got %q", m.filter)
	}

	// enter keeps the filter; esc from the list then clears it.
	m = press(t, m, special(tea.KeyEnter))
	if m.state != stateList || m.filter != "feat" {
		t.Fatalf("enter must keep the filter, got state=%v filter=%q", m.state, m.filter)
	}
	m = press(t, m, special(tea.KeyEsc))
	if m.filter != "" || len(m.viewIdx()) != 4 {
		t.Fatalf("esc must clear the filter, got %q (%d rows)", m.filter, len(m.viewIdx()))
	}
}

func TestFilterSelectAllMarksOnlyVisible(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}

	m = press(t, m, key("/"), key("unmerged"), special(tea.KeyEnter), key("a"), special(tea.KeyEsc))

	for _, b := range m.branches {
		want := b.name == "feature/unmerged"
		if b.selected != want {
			t.Fatalf("a under a filter must mark only visible branches: %s selected=%v", b.name, b.selected)
		}
	}
}

func TestFilterNoMatches(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}

	m = press(t, m, key("/"), key("zzz"))
	if out := stripANSI(m.listView()); !strings.Contains(out, "no branches match filter") {
		t.Fatalf("empty match must say so:\n%s", out)
	}
	// No visible row: cursor keys and actions must not panic or act.
	m = press(t, m, special(tea.KeyEnter), key(" "), key("c"), key("G"))
	if b := m.cur(); b != nil {
		t.Fatalf("no branch should be under the cursor, got %+v", b)
	}
	if got := currentBranch(t, repo); got != "main" {
		t.Fatalf("b with no row must not switch, got %q", got)
	}
}

func TestFilterCursorClamps(t *testing.T) {
	repo := setupRepo(t)
	chdir(t, repo)
	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	m = press(t, m, key("G"), key("/"), key("main"))
	if len(m.viewIdx()) != 1 || m.cursor != 0 {
		t.Fatalf("filter must clamp the cursor into view: cursor=%d rows=%d", m.cursor, len(m.viewIdx()))
	}
}

// withColor forces styled output for one test: go test has no TTY, so
// lipgloss otherwise renders nothing but plain text.
func withColor(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

// The cursor row carries a background band across the whole terminal width.
// Every column resets its own style, so the band must be re-armed after each
// reset or it would end at the first column boundary.
func TestCursorRowBand(t *testing.T) {
	withColor(t)
	m := model{width: 100}
	br := branch{name: "feature/x", upstream: "origin/feature/x", gone: true, subject: "wip"}
	on, _ := strings.CutSuffix(rowBgStyle.Render(""), "\x1b[0m")
	if on == "" {
		t.Fatal("precondition: the band style must render a background sequence")
	}

	row := m.renderRow(br, 12, true)
	if !strings.HasPrefix(row, on) || !strings.HasSuffix(row, "\x1b[0m") {
		t.Fatalf("cursor row must open the band and close it at the end:\n%q", row)
	}
	for _, seg := range strings.Split(strings.TrimSuffix(row, "\x1b[0m"), "\x1b[0m")[1:] {
		if !strings.HasPrefix(seg, on) {
			t.Fatalf("band must be re-armed after every reset, missing before %q", seg)
		}
	}
	if w := ansi.StringWidth(row); w != m.width {
		t.Fatalf("band must span the terminal: row is %d cells, want %d", w, m.width)
	}

	if plain := m.renderRow(br, 12, false); strings.Contains(plain, on) {
		t.Fatalf("only the cursor row carries the band:\n%q", plain)
	}
}

// A selected branch keeps its yellow name under the cursor: the band already
// marks the cursor, and the color is what says "this will be deleted".
func TestSelectedNameStaysYellowUnderCursor(t *testing.T) {
	withColor(t)
	m := model{width: 100}
	name := pad("feature/x", 12)
	br := branch{name: "feature/x", selected: true}
	row := m.renderRow(br, 12, true)
	if !strings.Contains(row, selStyle.Render(name)) {
		t.Fatalf("selected name must use the selection color under the cursor:\n%q", row)
	}
	if strings.Contains(row, cursorStyle.Render(name)) {
		t.Fatalf("selected name must not use the cursor color:\n%q", row)
	}
	// Unselected rows keep the cursor color on the name.
	br.selected = false
	if row := m.renderRow(br, 12, true); !strings.Contains(row, cursorStyle.Render(name)) {
		t.Fatalf("unselected cursor row must use the cursor color:\n%q", row)
	}
}

// x selects the gone branches p would select, without a fetch: risk-free ones
// only, never the current branch, with the same status wording.
func TestXSelectsSafeGoneBranches(t *testing.T) {
	m := model{width: 100, riskBase: "origin/main", branches: []branch{
		{name: "main", isCurrent: true},
		{name: "safe", upstream: "origin/safe", gone: true},
		{name: "risky", upstream: "origin/risky", gone: true, riskCommits: 2},
		{name: "live", upstream: "origin/live"},
		{name: "cur-gone", upstream: "origin/cur-gone", gone: true, isCurrent: true},
	}}
	m = press(t, m, key("x"))
	want := map[string]bool{"safe": true}
	for _, b := range m.branches {
		if b.selected != want[b.name] {
			t.Fatalf("branch %q selected=%v, want %v", b.name, b.selected, want[b.name])
		}
	}
	if !strings.Contains(m.status, "1 of 2 gone branch(es) selected") || !strings.Contains(m.status, "1 hold commits not in origin/main") {
		t.Fatalf("status must report the skipped branch, got %q", m.status)
	}

	m.branches = m.branches[:1]
	if m = press(t, m, key("x")); m.status != "no gone branches" {
		t.Fatalf("with nothing gone the status says so, got %q", m.status)
	}
}

// The diff view pipes through delta when it is on the PATH and shows its
// output as-is; without delta, or when delta fails, the raw lines come back
// for the app's own coloring so the diff is never lost.
func TestStyleDiffUsesDeltaWhenPresent(t *testing.T) {
	diff := "diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-old\n+new\n"
	bin := t.TempDir()
	t.Setenv("PATH", bin)

	lines, styled := styleDiff(diff, 80)
	if styled || len(lines) != 6 {
		t.Fatalf("without delta the raw lines must come back: styled=%v lines=%q", styled, lines)
	}
	if lines, styled := styleDiff("  \n", 80); styled || lines != nil {
		t.Fatalf("an empty diff has no lines: styled=%v lines=%q", styled, lines)
	}

	fake := filepath.Join(bin, "delta")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"delta $*\"\n/bin/cat >/dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	lines, styled = styleDiff(diff, 120)
	if !styled || len(lines) != 1 || lines[0] != "delta --paging=never --width=120" {
		t.Fatalf("delta must run with the terminal width and its output shown as-is: styled=%v lines=%q", styled, lines)
	}

	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if lines, styled := styleDiff(diff, 80); styled || len(lines) != 6 {
		t.Fatalf("a failing delta must fall back to the raw lines: styled=%v lines=%q", styled, lines)
	}
}

// Delta's columns are sized to the terminal, so a resize while the diff is
// open must run it again at the new width.
func TestDiffRerendersOnResize(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	if err := os.WriteFile(filepath.Join(bin, "delta"), []byte("#!/bin/sh\necho \"w $2\"\n/bin/cat >/dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := model{width: 80, height: 24, state: stateDiff, diffRaw: "-a\n+b\n"}
	m.diffLines, m.diffStyled = styleDiff(m.diffRaw, m.width)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m = nm.(model)
	if !m.diffStyled || len(m.diffLines) != 1 || m.diffLines[0] != "w --width=140" {
		t.Fatalf("resize must re-run delta at the new width, got %q", m.diffLines)
	}
	if o := stripANSI(m.diffView()); !strings.Contains(o, "· delta") || !strings.Contains(o, "w --width=140") {
		t.Fatalf("the view must show delta's lines and name it:\n%s", o)
	}
}

// result returns m's deletion result for the named branch, failing if none ran.
func result(t *testing.T, m model, name string) deleteResult {
	t.Helper()
	for _, r := range m.results {
		if r.br.name == name {
			return r
		}
	}
	t.Fatalf("no deletion result for %s: %+v", name, m.results)
	return deleteResult{}
}

// confirmBody returns the body lines of m's confirmation screen.
func (m model) confirmBody() []string {
	_, body, _ := m.confirmParts()
	return body
}

// realTempDir is t.TempDir with symlinks resolved, so its paths match the
// ones git reports (macOS puts temp dirs under /var, a link to /private/var).
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// dirExists reports whether path is present on disk.
func dirExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// git refuses to delete a branch any worktree has checked out, under -d and -D
// alike, so the delete has to remove the worktree first. That holds for a
// worktree whose directory was deleted by hand too: git still records it until
// it is pruned.
func TestWorktreeBranchDeletes(t *testing.T) {
	repo := setupLocalRepo(t)
	chdir(t, repo)
	git(t, repo, "branch", "feature/stale")
	wt := filepath.Join(realTempDir(t), "wt")
	stale := filepath.Join(realTempDir(t), "stale")
	git(t, repo, "worktree", "add", "-q", wt, "feature/merged")
	git(t, repo, "worktree", "add", "-q", stale, "feature/stale")
	if err := os.RemoveAll(stale); err != nil {
		t.Fatal(err)
	}

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"feature/merged", "feature/stale"} {
		b := find(m.branches, name)
		if b.worktree == "" || b.mainWorktree {
			t.Fatalf("precondition: %s should be held by a linked worktree: %+v", name, b)
		}
		b.selected = true
	}
	m.force = false
	m = startAndDrain(t, m, false)

	wantState(t, m, stateResult, "both deletes should succeed")
	for _, name := range []string{"feature/merged", "feature/stale"} {
		if r := result(t, m, name); !r.worktreeRemoved || !r.localOK {
			t.Fatalf("%s: want worktree removed and branch deleted: %+v", name, r)
		}
		if find(m.branches, name) != nil {
			t.Fatalf("%s should be gone after the delete", name)
		}
	}
	if dirExists(wt) {
		t.Fatal("the worktree directory should be removed")
	}
	if out := git(t, repo, "worktree", "list", "--porcelain"); strings.Contains(out, "feature/") {
		t.Fatalf("git should no longer record either worktree:\n%s", out)
	}
	if body := strings.Join(appendResultLines(nil, result(t, m, "feature/merged")), "\n"); !strings.Contains(body, "removed worktree "+wt) {
		t.Fatalf("the results must name the removed worktree:\n%s", body)
	}
}

// A worktree with uncommitted work survives a safe delete, and the branch with
// it; force mode is the user's say-so to discard that work.
func TestDirtyWorktreeNeedsForce(t *testing.T) {
	repo := setupLocalRepo(t)
	chdir(t, repo)
	wt := filepath.Join(realTempDir(t), "wt")
	git(t, repo, "worktree", "add", "-q", wt, "feature/merged")
	if err := os.WriteFile(filepath.Join(wt, "scratch"), []byte("unsaved work"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	find(m.branches, "feature/merged").selected = true
	m.force = false
	if body := strings.Join(m.confirmBody(), "\n"); !strings.Contains(body, "remove worktree "+wt+" (refused if it has uncommitted changes)") {
		t.Fatalf("the safe confirm must say the worktree goes only if clean:\n%s", body)
	}
	m = startAndDrain(t, m, false)

	wantState(t, m, stateResult, "a dirty worktree is not a force-prompt case")
	if r := result(t, m, "feature/merged"); r.localOK || r.worktreeRemoved || !strings.Contains(r.localErr, wt) {
		t.Fatalf("safe mode must refuse the dirty worktree and name it: %+v", r)
	}
	if !dirExists(filepath.Join(wt, "scratch")) || find(m.branches, "feature/merged") == nil {
		t.Fatal("the worktree, its files and the branch must all survive a refused delete")
	}

	find(m.branches, "feature/merged").selected = true
	m.force = true
	if body := strings.Join(m.confirmBody(), "\n"); !strings.Contains(body, "uncommitted changes will be lost") {
		t.Fatalf("the force confirm must warn that changes are discarded:\n%s", body)
	}
	m = startAndDrain(t, m, false)
	if r := result(t, m, "feature/merged"); !r.localOK || !r.worktreeRemoved {
		t.Fatalf("force mode should remove the dirty worktree and the branch: %+v", r)
	}
	if dirExists(wt) {
		t.Fatal("the worktree directory should be removed under force")
	}
}

// When -d is going to refuse the branch as unmerged, removing its worktree
// first would leave the user with neither: the branch survives, the worktree
// does not. The worktree stays until the user accepts the force retry.
func TestUnmergedWorktreeKeptUntilForce(t *testing.T) {
	repo := setupLocalRepo(t)
	chdir(t, repo)
	wt := filepath.Join(realTempDir(t), "wt")
	git(t, repo, "worktree", "add", "-q", wt, "feature/unmerged")

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	find(m.branches, "feature/unmerged").selected = true
	m.force = false
	m = startAndDrain(t, m, false)

	wantState(t, m, stateForcePrompt, "an unmerged branch must still raise the force prompt")
	if r := result(t, m, "feature/unmerged"); r.worktreeRemoved || !r.forceable {
		t.Fatalf("the worktree must be kept and the delete forceable: %+v", r)
	}
	if !dirExists(wt) {
		t.Fatal("the worktree must survive the refused -d")
	}
	if _, body, _ := m.forcePromptParts(); !strings.Contains(strings.Join(body, "\n"), "worktree "+wt+" will be removed first") {
		t.Fatalf("the force prompt must name the worktree it removes:\n%s", strings.Join(body, "\n"))
	}

	m = press(t, m, key("y"))
	if r := result(t, m, "feature/unmerged"); !r.localOK || !r.worktreeRemoved {
		t.Fatalf("the force retry should remove the worktree and the branch: %+v", r)
	}
	if dirExists(wt) || find(m.branches, "feature/unmerged") != nil {
		t.Fatal("the worktree and the branch should both be gone after the force retry")
	}
}

// Run from a linked worktree, the branch checked out in the main worktree is as
// undeletable as the current one: git cannot remove the main worktree to free it.
func TestMainWorktreeBranchCannotBeMarked(t *testing.T) {
	repo := setupLocalRepo(t)
	addOrigin(t, repo, "main")
	git(t, repo, "push", "-q", "-u", "origin", "feature/merged")
	git(t, repo, "branch", "-q", "-u", "origin/feature/merged", "main")
	git(t, repo, "push", "-q", "origin", "--delete", "feature/merged")
	git(t, repo, "fetch", "-q", "--prune")
	wt := filepath.Join(realTempDir(t), "wt")
	git(t, repo, "worktree", "add", "-q", wt, "feature/unmerged")
	chdir(t, wt)

	m, err := initialModel()
	if err != nil {
		t.Fatal(err)
	}
	b := find(m.branches, "main")
	if b == nil || !b.mainWorktree || b.isCurrent {
		t.Fatalf("precondition: main should be held by the main worktree: %+v", b)
	}
	for i := range m.branches {
		if m.branches[i].name == "main" {
			m.cursor = i
		}
	}
	m = press(t, m, key(" "), key("a"), key("x"))
	for _, b := range m.selectedBranches() {
		if b.name == "main" || b.isCurrent {
			t.Fatalf("%s must not be selectable from a linked worktree", b.name)
		}
	}
	if row := m.renderRow(*find(m.branches, "main"), 12, false); !strings.Contains(row, worktreeStyle.Render("+")) {
		t.Fatalf("a branch held by another worktree must show the + marker:\n%q", row)
	}
}
