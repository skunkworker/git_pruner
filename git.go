package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// branch holds the metadata git_pruner displays and acts on for one local
// branch, or for one remote branch with no local copy (remoteOnly).
type branch struct {
	name         string // "feature"; for a remoteOnly row, the tracking name "origin/feature"
	hash         string
	sha          string // full commit ID; what undo recreates the branch at
	subject      string
	committed    time.Time
	committedRel string
	upstream     string // e.g. "origin/feature"; "" when no upstream is configured
	upRemote     string // branch.<name>.remote, kept so undo can restore tracking
	upMerge      string // branch.<name>.merge, e.g. "refs/heads/feature"
	ahead        int
	behind       int
	gone         bool // upstream was configured but no longer exists
	remoteMerged bool // upstream is merged into the remote default branch (safe to delete)
	headMerged   bool // branch tip is merged into HEAD (git's -d criterion when there is no upstream)
	riskCommits  int  // commits whose patch is not in the base branch; -D discards them
	riskMeasured bool // riskCommits has been computed (0 is a meaningful value)
	isCurrent    bool
	worktree     string // path of another worktree that has the branch checked out; "" when none does
	mainWorktree bool   // that worktree is the main one, which git cannot remove
	protected    bool   // matches the default branch or a pruner.protect pattern
	remoteOnly   bool   // a remote branch that no local branch tracks
	selected     bool
	deleteRemote bool
}

// remoteArmed reports whether deleting b includes its remote branch. Selecting
// a remoteOnly row arms it: deleting the remote branch is all it offers.
func (b branch) remoteArmed() bool { return (b.deleteRemote || b.remoteOnly) && b.upstream != "" }

// locked reports whether b can never be marked for deletion. A branch in a
// linked worktree is not: the delete removes the worktree first. The main
// worktree cannot be removed, so its branch stays locked.
func (b branch) locked() bool { return b.isCurrent || b.mainWorktree || b.protected }

// ref is b's fully qualified ref (see branchRef).
func (b branch) ref() string {
	if b.remoteOnly {
		return "refs/remotes/" + b.name
	}
	return branchRef(b.name)
}

// remoteName prefers the configured remote: it is the truth, while the tracking
// name is only a convention that custom refspecs break.
func (b branch) remoteName() string {
	if b.upRemote != "" && b.upRemote != "." {
		return b.upRemote
	}
	if name, _, ok := strings.Cut(b.upstream, "/"); ok {
		return name
	}
	return "origin"
}

func (b branch) remoteBranch() string {
	if s, ok := strings.CutPrefix(b.upMerge, "refs/heads/"); ok {
		return s
	}
	if _, name, ok := strings.Cut(b.upstream, "/"); ok {
		return name
	}
	return b.name
}

// safeDeletable reports whether `git branch -d` will accept b, mirroring git's
// rule: a resolvable upstream is the sole criterion (ahead == 0 means the branch
// holds no commit the upstream lacks), and HEAD is consulted only when there is
// no upstream to ask. This is a precedence, not an either-or — git refuses a
// branch ahead of its upstream even when HEAD already contains it. Note that a
// branch with no upstream always has ahead == 0, git reporting no track info for
// it, so ahead alone cannot answer this.
func (b branch) safeDeletable() bool {
	if b.upstream != "" && !b.gone {
		return b.ahead == 0
	}
	return b.headMerged
}

// forcedDelete reports whether b will be deleted with -D rather than -d. Gone
// branches always are: git reports no track info for them, so a safe delete
// would turn on HEAD alone and refuse branches whose work is in the remote
// default but not in the local checkout — the headline prune case.
func (b branch) forcedDelete(force bool) bool { return force || b.gone }

// safeDeleteRefuses reports whether the delete of b under the given force mode
// will run -d and be refused as not fully merged.
func (b branch) safeDeleteRefuses(force bool) bool {
	return !b.forcedDelete(force) && !b.safeDeletable()
}

// Concurrency caps. Local git work is subprocess-bound — roughly 6ms of spawn
// cost each — so running it a few at a time is what makes a repo full of gone
// branches load quickly. Remote work is not: every push or fetch opens its own
// connection, and a prune of a hundred armed branches would open a hundred at
// once, which remotes throttle or refuse. The two are capped separately so a
// wide local fan-out never widens the network one.
const (
	maxLocalGit   = 8
	maxRemotePush = 3
)

var (
	localSlots  = make(chan struct{}, maxLocalGit)
	remoteSlots = make(chan struct{}, maxRemotePush)
)

// networkBound reports whether a git invocation opens a connection to a remote.
// It is what picks the cap, so a new network subcommand belongs here rather than
// at its call site.
func networkBound(args []string) bool {
	return len(args) > 0 && (args[0] == "push" || args[0] == "fetch")
}

// gauge records the highest number of concurrent holders it has seen. It sits on
// the subprocesses rather than on the slots, so a test can tell a cap that works
// from one that was removed — instrumenting the cap would stop reporting along
// with it.
// It also counts every holder, which is what tells a fan-out that was removed
// from one that merely runs fast on a small fixture.
type gauge struct{ inFlight, peak, total atomic.Int64 }

func (g *gauge) enter() {
	g.total.Add(1)
	n := g.inFlight.Add(1)
	for {
		peak := g.peak.Load()
		if n <= peak || g.peak.CompareAndSwap(peak, n) {
			return
		}
	}
}

func (g *gauge) leave() { g.inFlight.Add(-1) }

// gitProcs counts every git subprocess; netProcs counts the ones that talk to a
// remote, which is what a remote host actually feels.
var gitProcs, netProcs gauge

// runGit is the single door every git invocation passes through, which is what
// makes it the place to bound them: a cap at the call sites would only hold for
// the callers that remembered to ask.
func runGit(args ...string) (string, error) {
	net := networkBound(args)
	slots := localSlots
	if net {
		slots = remoteSlots
	}
	slots <- struct{}{}
	defer func() { <-slots }()

	// Counted after the slot is held, so the gauges measure what is running
	// rather than what is queued.
	gitProcs.enter()
	defer gitProcs.leave()
	if net {
		netProcs.enter()
		defer netProcs.leave()
	}

	ctx := context.Background()
	if net {
		// A remote that stops answering would otherwise hold the slot, and the
		// screen waiting on it, forever.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, netTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	// Pin the locale: deleteBranch classifies failures by matching git's own
	// error text, which gettext would otherwise translate. Everything else we
	// parse is --format-driven and unaffected.
	// GIT_TERMINAL_PROMPT=0 makes git fail instead of asking for a password: the
	// TUI owns the terminal, so a prompt there can never be answered.
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	if net {
		// ssh asks for passphrases on /dev/tty, past GIT_TERMINAL_PROMPT. With no
		// controlling terminal it fails fast instead of fighting the TUI for input.
		detachTTY(cmd)
	}
	// Once git is killed, an ssh child can still hold the pipes open; stop
	// waiting on them shortly after.
	cmd.WaitDelay = 2 * time.Second
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if ctx.Err() == context.DeadlineExceeded {
			msg = fmt.Sprintf("git %s timed out after %s (the remote did not answer, or asked for a password)", args[0], netTimeout)
		} else if msg == "" {
			msg = err.Error()
		}
		return strings.TrimSpace(out.String()), fmt.Errorf("%s", cleanText(msg))
	}
	return out.String(), nil
}

// netTimeout bounds each network git call. A var so tests can shorten it.
var netTimeout = 60 * time.Second

// cleanText makes text from git safe to print in the TUI. Commit subjects and
// remote error messages come from other people, so escape codes in them could
// recolor or move the screen. Line breaks and tabs become spaces: every place
// this text lands is one row.
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, ansi.Strip(s))
}

// branchRef fully qualifies a local branch name. git's ref search order puts
// refs/tags/<name> ahead of refs/heads/<name>, so a tag sharing a branch's name
// silently shadows the branch in any command handed the bare name — and tagging a
// release branch with its own name is ordinary practice.
func branchRef(name string) string { return "refs/heads/" + name }

// shortRef strips the namespace from a full ref, yielding the plain branch name
// ("feature/x") or remote-tracking name ("origin/feature/x") the rest of the
// program keys on. git's own %(refname:short) cannot be used for this: it yields
// the shortest *unambiguous* name, which grows a "heads/" or "remotes/" prefix
// exactly when a tag shares the name — silently breaking every name-keyed lookup
// and every ref built back up from it.
func shortRef(ref string) string {
	for _, prefix := range []string{"refs/heads/", "refs/remotes/"} {
		if s, ok := strings.CutPrefix(ref, prefix); ok {
			return s
		}
	}
	return ref
}

// refExists reports whether a ref resolves. Always pass a fully qualified ref: a
// bare name would also match a tag (see branchRef).
func refExists(ref string) bool {
	_, err := runGit("rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

var trackRe = regexp.MustCompile(`ahead (\d+)|behind (\d+)`)

// commitFormat is the for-each-ref format shared by the local and remote
// loaders. The subject goes last: it is the one free-text field.
const commitFormat = "%(refname)%00%(objectname:short)%00%(objectname)%00" +
	"%(committerdate:iso8601-strict)%00%(committerdate:relative)%00%(contents:subject)"

// parseCommitFields reads commitFormat's fields into a branch.
func parseCommitFields(f []string) branch {
	b := branch{
		name:         shortRef(f[0]),
		hash:         f[1],
		sha:          f[2],
		committedRel: f[4],
		subject:      cleanText(f[5]),
	}
	if t, err := time.Parse(time.RFC3339, f[3]); err == nil {
		b.committed = t
	}
	return b
}

func loadBranches() ([]branch, error) {
	// Branch-only fields go first, so commitFormat's fields close the line.
	const format = "%(upstream)%00%(upstream:track)%00%(HEAD)%00%(worktreepath)%00" +
		"%(upstream:remotename)%00%(upstream:remoteref)%00" + commitFormat
	out, err := runGit("for-each-ref", "--format="+format, "refs/heads")
	if err != nil {
		return nil, err
	}
	var branches []branch
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\x00")
		if len(f) < 12 {
			continue
		}
		b := parseCommitFields(f[6:])
		b.upstream = shortRef(f[0])
		b.isCurrent = f[2] == "*"
		// %(worktreepath) is set for the current worktree's branch too.
		if !b.isCurrent {
			b.worktree = f[3]
		}
		b.upRemote, b.upMerge = f[4], f[5]
		track := f[1]
		if strings.Contains(track, "gone") {
			b.gone = true
		}
		for _, mm := range trackRe.FindAllStringSubmatch(track, -1) {
			if mm[1] != "" {
				b.ahead, _ = strconv.Atoi(mm[1])
			}
			if mm[2] != "" {
				b.behind, _ = strconv.Atoi(mm[2])
			}
		}
		branches = append(branches, b)
	}
	return branches, nil
}

// remoteRefs is one read of refs/remotes: which remote-tracking refs exist, and
// what each symbolic one points at. Reading them together is what replaces `git
// remote`, a `symbolic-ref` per remote and a `rev-parse --verify` per candidate
// — five subprocess starts on an ordinary one-remote repo, against this one.
type remoteRefs struct {
	names  []string          // remote names, "origin" first
	exists map[string]bool   // fully qualified ref -> present
	symref map[string]string // fully qualified symbolic ref -> the ref it names
	rows   []branch          // every non-symbolic remote branch; only when asked for
}

// loadRemoteRefs reads every remote-tracking ref in one call. The remote names
// come out of the refs rather than out of `git remote`: a remote with no fetched
// refs cannot supply a default branch, so it is nothing the caller could use.
// withRows also reads each ref's commit for the remote view. That costs a commit
// read per ref, so it is only paid once the user has opened that view.
func loadRemoteRefs(withRows bool) remoteRefs {
	rr := remoteRefs{exists: map[string]bool{}, symref: map[string]string{}}
	format := "%(symref)%00%(refname)"
	if withRows {
		format = "%(symref)%00" + commitFormat
	}
	out, err := runGit("for-each-ref", "--format="+format, "refs/remotes")
	if err != nil {
		return rr
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "\x00")
		if len(f) < 2 || f[1] == "" {
			continue
		}
		ref, target := f[1], f[0]
		rr.exists[ref] = true
		if target != "" {
			rr.symref[ref] = target
		} else if withRows && len(f) >= 7 {
			b := parseCommitFields(f[1:])
			b.remoteOnly = true
			// The row is its own upstream, which lets the merge query and the
			// push --delete path treat it like a local branch's remote copy.
			b.upstream = b.name
			rr.rows = append(rr.rows, b)
		}
		// The first segment after the namespace is the remote's name.
		if name, _, ok := strings.Cut(shortRef(ref), "/"); ok && !seen[name] {
			seen[name] = true
			rr.names = append(rr.names, name)
		}
	}
	// origin first, so the conventional remote wins when several exist while a
	// repo whose only remote is named something else (upstream, fork, …) still
	// resolves a default branch.
	sort.SliceStable(rr.names, func(i, j int) bool { return rr.names[i] == "origin" && rr.names[j] != "origin" })
	return rr
}

// defaultBranchNames are the branch names treated as a repo's trunk, in order of
// preference.
var defaultBranchNames = []string{"main", "master"}

// localDefaultBranch returns the ref of a local main/master, skipping exclude (a
// short branch name) so a branch is never compared against itself. Returns ""
// when neither exists. has answers whether a local branch of that name exists;
// a caller already holding the branch list passes a lookup into it rather than
// paying a subprocess per candidate.
//
// The resolvers below all return fully qualified refs, and the display layer
// shortens them with shortRef. Resolving is the only place the namespace is
// known for certain, so carrying it forward from here is what keeps a same-named
// tag from being measured in place of the branch further down.
func localDefaultBranch(exclude string, has func(string) bool) string {
	for _, c := range defaultBranchNames {
		if c != exclude && has(c) {
			return branchRef(c)
		}
	}
	return ""
}

// gitHasBranch is the localDefaultBranch lookup for callers with no branch list
// to hand — it costs a subprocess per candidate.
func gitHasBranch(name string) bool { return refExists(branchRef(name)) }

// remoteDefaultFrom resolves the remote's default branch as a remote-tracking
// ref (e.g. "refs/remotes/origin/main"): <remote>/HEAD if set, else
// <remote>/main, else <remote>/master, trying each remote in turn. Returns ""
// when none can be found.
func remoteDefaultFrom(rr remoteRefs) string {
	for _, r := range rr.names {
		// %(symref) yields the full ref, unlike `symbolic-ref --short`, which
		// gives the shortest *unambiguous* name — "remotes/origin/main" as soon
		// as a tag shares the name.
		if t := rr.symref["refs/remotes/"+r+"/HEAD"]; t != "" {
			return t
		}
		for _, c := range defaultBranchNames {
			if ref := "refs/remotes/" + r + "/" + c; rr.exists[ref] {
				return ref
			}
		}
	}
	return ""
}

// remoteDefault reads the remote-tracking refs and resolves the default branch
// from them.
func remoteDefault() string { return remoteDefaultFrom(loadRemoteRefs(false)) }

// baseBranch returns the ref to diff a branch against: the remote default branch,
// else a local main/master, excluding name itself.
func baseBranch(name string) string {
	if def := remoteDefault(); def != "" {
		return def
	}
	return localDefaultBranch(name, gitHasBranch)
}

// riskCommitCount counts commits on name whose patch is not already present in
// base — the work a force delete (-D) would discard. Uses `git cherry` rather
// than `rev-list base..name` so commits that were cherry-picked, rebased, or
// squashed singly into base are correctly seen as already integrated. Commits
// squashed as a group still count, since no equivalent single patch exists;
// the warning is therefore worded as "not in <base>", not "will be lost".
// Returns 0 when there is nothing to compare against.
func riskCommitCount(name, base string) int { return riskCommitCountRef(branchRef(name), base) }

// riskCommitCountRef is riskCommitCount for any fully qualified ref, which is
// what lets a remote branch be measured the same way.
func riskCommitCountRef(ref, base string) int {
	if base == "" || base == ref {
		return 0
	}
	out, err := runGit("cherry", base, ref)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "+") { // '+' = no equivalent patch in base
			n++
		}
	}
	return n
}

// mergedSet runs a `git branch --merged` query and collects the short ref names
// it reports into a set.
func mergedSet(args ...string) map[string]bool {
	set := map[string]bool{}
	out, err := runGit(append(args, "--format=%(refname)")...)
	if err != nil {
		return set
	}
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			set[shortRef(s)] = true
		}
	}
	return set
}

// remoteMergedSet returns the set of remote-tracking branches (short names, e.g.
// "origin/feature") whose tip is merged into def, a qualified ref. Operates on
// local remote-tracking refs, so it needs no network — it reflects the last fetch.
func remoteMergedSet(def string) map[string]bool {
	if def == "" {
		return map[string]bool{}
	}
	return mergedSet("branch", "-r", "--merged", def)
}

// localMergedSet returns the local branches whose tip is merged into HEAD —
// git's criterion for accepting `branch -d` on a branch with no upstream. One
// git call covers the whole list, so this costs nothing per branch.
func localMergedSet() map[string]bool { return mergedSet("branch", "--merged", "HEAD") }

// repoReads holds the reads that do not depend on the branch list, so they can
// run in the same round as it.
type repoReads struct {
	headMerged map[string]bool // local branches merged into HEAD
	remotes    remoteRefs
	config     prunerConfig
}

// prunerConfig is the tool's own git config, under the pruner.* section.
type prunerConfig struct {
	protect   []string // pruner.protect: branch name globs that can never be marked
	staleDays int      // pruner.staleDays: the default age for the merged-and-old rule
}

// defaultStaleDays is the age the merged-and-old rule starts from.
const defaultStaleDays = 90

// loadConfig reads every pruner.* key in one call. Unset is the usual case, and
// git exits 1 for it, so an error only means "use the defaults".
func loadConfig() prunerConfig {
	c := prunerConfig{staleDays: defaultStaleDays}
	out, _ := runGit("config", "--get-regexp", `^pruner\.`)
	for _, line := range strings.Split(out, "\n") {
		key, val, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch key {
		case "pruner.protect":
			c.protect = append(c.protect, strings.Fields(val)...)
		case "pruner.staledays":
			if n, err := strconv.Atoi(val); err == nil && n >= 0 {
				c.staleDays = n
			}
		}
	}
	return c
}

// loadRepo reads the branch list and everything independent of it in one round.
// Starting a git subprocess costs about 6ms, and none of these reads waits on
// another, so the depth of the chain is what the user waits on — not the work
// inside it.
func loadRepo(withRemote bool) ([]branch, repoReads, error) {
	var (
		branches []branch
		err      error
		reads    repoReads
		mainWT   string
	)
	var wg sync.WaitGroup
	wg.Add(5)
	go func() { defer wg.Done(); branches, err = loadBranches() }()
	go func() { defer wg.Done(); reads.headMerged = localMergedSet() }()
	go func() { defer wg.Done(); reads.remotes = loadRemoteRefs(withRemote) }()
	go func() { defer wg.Done(); reads.config = loadConfig() }()
	go func() { defer wg.Done(); mainWT = mainWorktreePath() }()
	wg.Wait()
	for i := range branches {
		branches[i].mainWorktree = mainWT != "" && branches[i].worktree == mainWT
	}
	return branches, reads, err
}

// mainWorktreePath returns the path of the repository's main worktree, which git
// always lists first. "" when the list cannot be read.
func mainWorktreePath() string {
	out, err := runGit("worktree", "list", "--porcelain", "-z")
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(out, "\x00")
	if path, ok := strings.CutPrefix(first, "worktree "); ok {
		return path
	}
	return ""
}

// loadDiff returns the patch introduced on name relative to its merge-base with
// the repo's default branch — i.e. what the branch contains — and the base it was
// compared against, shortened for display.
func loadDiff(name string) (diff, base string, err error) { return loadDiffRef(branchRef(name), name) }

// loadDiffRef is loadDiff for any fully qualified ref; name is the short name
// that must not be picked as its own base.
func loadDiffRef(ref, name string) (diff, base string, err error) {
	baseRef := baseBranch(name)
	if baseRef == "" || baseRef == ref {
		baseRef = "HEAD"
	}
	diff, err = runGit("diff", baseRef+"..."+ref)
	return diff, shortRef(baseRef), err
}

// styleDiff turns a unified diff into display lines. With delta on the PATH
// its output is used as-is, so side-by-side, line numbers and theme all come
// from the user's own delta config; the width is passed so delta lays the
// columns out for this terminal. Without delta (or if it fails) the raw lines
// are returned for colorizeDiffLine.
func styleDiff(diff string, width int) (lines []string, styled bool) {
	if strings.TrimSpace(diff) == "" {
		return nil, false
	}
	if path, err := exec.LookPath("delta"); err == nil {
		cmd := exec.Command(path, "--paging=never", "--width="+strconv.Itoa(width))
		cmd.Stdin = strings.NewReader(diff)
		if out, err := cmd.Output(); err == nil {
			return strings.Split(strings.TrimRight(string(out), "\n"), "\n"), true
		}
	}
	// The file contents are someone else's text; clean each line like a subject.
	// Tabs are widened first, or cleanText would flatten indentation to one space.
	lines = strings.Split(strings.TrimRight(diff, "\n"), "\n")
	for i, l := range lines {
		lines[i] = cleanText(strings.ReplaceAll(l, "\t", "    "))
	}
	return lines, false
}

// deleteFlag returns the git branch delete flag for b under the given force mode.
func (b branch) deleteFlag(force bool) string {
	if b.forcedDelete(force) {
		return "-D"
	}
	return "-d"
}

// deleteBranch runs one branch's local delete and, when wantRemote is set, its
// remote-branch push --delete. It is the worker deleteBranchCmd runs off the
// update loop, one cmd per branch. force is the user's force mode, which picks
// the delete flag.
func deleteBranch(b branch, force, wantRemote bool) deleteResult {
	res := deleteResult{br: b, done: true}
	if b.remoteOnly {
		// There is no local branch; the push is the whole job.
		if wantRemote {
			pushRemoteDelete(&res)
		}
		return res
	}
	flag := b.deleteFlag(force)
	switch {
	case b.worktree != "" && b.safeDeleteRefuses(force):
		// git checks the worktree before the merge, so -d would report the
		// worktree, not the unmerged commits that the force retry can clear.
		// The worktree stays until the user agrees to that retry.
		res.localErr = "not fully merged (worktree kept)"
		res.forceable = true
	case b.worktree != "" && !removeWorktree(&res):
	default:
		if _, err := runGit("branch", flag, b.name); err != nil {
			res.localErr = err.Error()
			// Only an unmerged refusal is worth escalating to -D. Other
			// failures fail identically under -D, so offering the retry would
			// just mislabel them as lost commits.
			res.forceable = flag == "-d" && strings.Contains(res.localErr, "not fully merged")
		} else {
			res.localOK = true
		}
	}
	// Never delete the remote copy while the local branch survives a refused
	// delete: that would strand its commits with nowhere else to exist. The push
	// is deferred until the force retry clears the local branch.
	switch {
	case !wantRemote:
	case res.localOK:
		pushRemoteDelete(&res)
	default:
		res.remoteSkipped = true
	}
	return res
}

// removeWorktree removes the linked worktree holding res's branch: git refuses to
// delete a branch any worktree has checked out, under -d and -D alike. A
// worktree is scratch space for its branch, so it goes with everything in it,
// uncommitted changes included. A locked worktree is refused: someone locked it
// on purpose. It reports whether the branch is now free to delete.
func removeWorktree(res *deleteResult) bool {
	if _, err := runGit("worktree", "remove", "--force", res.br.worktree); err != nil {
		res.localErr = "worktree " + res.br.worktree + ": " + cleanText(err.Error())
		return false
	}
	res.worktreeRemoved = true
	return true
}

// pushRemoteDelete deletes res's remote branch, clearing any deferral: the
// results screen tests remoteSkipped first, so a stale flag would report the
// remote as kept right after a successful push.
func pushRemoteDelete(res *deleteResult) {
	res.remoteSkipped = false
	res.remoteTried = true
	// Qualify the remote branch: a remote carrying both a branch and a tag of that
	// name rejects a bare refspec as matching more than one ref, deleting nothing.
	if _, err := runGit("push", res.br.remoteName(), "--delete", branchRef(res.br.remoteBranch())); err != nil {
		res.remoteErr = err.Error()
	} else {
		res.remoteOK = true
	}
}

// restoreBranch recreates a deleted local branch at its old commit and puts its
// upstream config back; `git branch -d` removes both. The commit is still in the
// object store (git only prunes unreachable objects after weeks), so this works
// long after the delete.
func restoreBranch(b branch) error {
	if _, err := runGit("branch", "--no-track", b.name, b.sha); err != nil {
		return err
	}
	if b.upRemote == "" || b.upMerge == "" {
		return nil
	}
	if _, err := runGit("config", "branch."+b.name+".remote", b.upRemote); err != nil {
		return err
	}
	_, err := runGit("config", "branch."+b.name+".merge", b.upMerge)
	return err
}
