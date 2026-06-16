package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

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
