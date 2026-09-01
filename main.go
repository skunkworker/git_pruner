package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// branch holds the metadata git_pruner displays and acts on for one local branch.
type branch struct {
	name         string
	hash         string
	subject      string
	committed    time.Time
	committedRel string
	upstream     string // e.g. "origin/feature"; "" when no upstream is configured
	ahead        int
	behind       int
	gone         bool // upstream was configured but no longer exists
	remoteMerged bool // upstream is merged into the remote default branch (safe to delete)
	headMerged   bool // branch tip is merged into HEAD (git's -d criterion when there is no upstream)
	riskCommits  int  // commits whose patch is not in the base branch; -D discards them
	riskMeasured bool // riskCommits has been computed (0 is a meaningful value)
	isCurrent    bool
	selected     bool
	deleteRemote bool
}

func (b branch) remoteName() string {
	if name, _, ok := strings.Cut(b.upstream, "/"); ok {
		return name
	}
	return "origin"
}

func (b branch) remoteBranch() string {
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

type sortField int

const (
	sortDate sortField = iota
	sortName
	sortAheadBehind
	sortFieldCount // number of sort fields; keep last
)

func (s sortField) String() string {
	switch s {
	case sortDate:
		return "committerdate"
	case sortName:
		return "name"
	case sortAheadBehind:
		return "ahead/behind"
	}
	return "?"
}

type viewState int

const (
	stateList viewState = iota
	stateConfirm
	stateForcePrompt
	stateDeleting
	stateResult
	stateHelp
	stateDiff
	stateFilter // the list, with the filter line capturing keystrokes
)

type deleteResult struct {
	br          branch // the branch this deletion was run for
	done        bool   // the async deletion for this branch has completed
	localOK     bool
	localErr    string
	forceable   bool // a safe (-d) delete failed and could be retried with -D
	remoteTried bool
	remoteOK    bool
	remoteErr   string
	// remoteSkipped records that the armed push was deliberately deferred
	// because the local delete failed — the one piece of state not derivable
	// from br, since arming is the caller's decision.
	remoteSkipped bool
}

type model struct {
	branches []branch
	cursor   int
	top      int // index of first visible row (scroll window)

	field     sortField
	ascending bool
	force     bool
	nameW     int // cached branch-name column width (see recomputeNameWidth)

	filter string // active branch-name filter; "" shows every branch

	state   viewState
	results []deleteResult

	remoteDefault string // resolved remote default branch, e.g. "origin/main"
	riskBase      string // ref that branch.riskCommits is measured against ("" if unresolved)
	riskBaseRef   string // riskBase fully qualified, so a same-named tag cannot shadow it
	// baseMerged holds the local branches whose tip is an ancestor of riskBaseRef.
	// Their riskCommits is 0 by definition, so one query here removes a `git
	// cherry` subprocess per branch (see measureRisk).
	baseMerged map[string]bool

	spinnerFrame int // animation frame for the deleting spinner (deletion counts derive from results)

	diffBranch string   // branch whose diff is shown in stateDiff
	diffBase   string   // base ref the diff was computed against
	diffLines  []string // raw lines of the diff being viewed
	diffTop    int      // scroll offset within diffLines

	bodyTop int // scroll offset within the confirm/force/result body (see page)

	width, height int
	err           string
	status        string // transient info message (e.g. fetch results)
	fetching      bool   // a background fetch --all --prune is in flight
	switching     bool   // a background git switch is in flight
}

// ---- styles ----

var (
	currentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	cursorStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	selStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	goneStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	headerStyle  = lipgloss.NewStyle().Bold(true)
	okStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))

	nameStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("14")) // cyan
	hashStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))  // yellow
	subjectStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))  // light gray
	aheadStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("10")) // green
	behindStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))  // red
	trackColStyle = lipgloss.NewStyle().Width(10)                        // ahead/behind + optional merged ✓

	addStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("10")) // green: additions
	delStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))  // red: removals
	hunkStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("13")) // magenta: hunk headers
)

// ---- git I/O ----

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

	cmd := exec.Command("git", args...)
	// Pin the locale: deleteBranch classifies failures by matching git's own
	// error text, which gettext would otherwise translate. Everything else we
	// parse is --format-driven and unaffected.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return strings.TrimSpace(out.String()), fmt.Errorf("%s", msg)
	}
	return out.String(), nil
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

func loadBranches() ([]branch, error) {
	const format = "%(refname)%00%(objectname:short)%00%(committerdate:iso8601-strict)%00" +
		"%(committerdate:relative)%00%(upstream)%00%(upstream:track)%00%(HEAD)%00%(contents:subject)"
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
		if len(f) < 8 {
			continue
		}
		b := branch{
			name:         shortRef(f[0]),
			hash:         f[1],
			committedRel: f[3],
			upstream:     shortRef(f[4]),
			isCurrent:    f[6] == "*",
			subject:      f[7],
		}
		if t, terr := time.Parse(time.RFC3339, f[2]); terr == nil {
			b.committed = t
		}
		track := f[5]
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
}

// loadRemoteRefs reads every remote-tracking ref in one call. The remote names
// come out of the refs rather than out of `git remote`: a remote with no fetched
// refs cannot supply a default branch, so it is nothing the caller could use.
func loadRemoteRefs() remoteRefs {
	rr := remoteRefs{exists: map[string]bool{}, symref: map[string]string{}}
	out, err := runGit("for-each-ref", "--format=%(refname)%00%(symref)", "refs/remotes")
	if err != nil {
		return rr
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		ref, target := "", ""
		if s := strings.TrimSpace(line); s != "" {
			ref, target, _ = strings.Cut(s, "\x00")
		}
		if ref == "" {
			continue
		}
		rr.exists[ref] = true
		if target != "" {
			rr.symref[ref] = target
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
func remoteDefault() string { return remoteDefaultFrom(loadRemoteRefs()) }

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
func riskCommitCount(name, base string) int {
	ref := branchRef(name)
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
}

// loadRepo reads the branch list and everything independent of it in one round.
// Starting a git subprocess costs about 6ms, and none of these three waits on
// another, so the depth of the chain is what the user waits on — not the work
// inside it.
func loadRepo() ([]branch, repoReads, error) {
	var (
		branches []branch
		err      error
		reads    repoReads
	)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); branches, err = loadBranches() }()
	go func() { defer wg.Done(); reads.headMerged = localMergedSet() }()
	go func() { defer wg.Done(); reads.remotes = loadRemoteRefs() }()
	wg.Wait()
	return branches, reads, err
}

// fetchDoneMsg reports completion of an async `git fetch --all --prune`.
type fetchDoneMsg struct{ err error }

// fetchPruneCmd fetches all remotes and prunes deleted remote-tracking refs so
// that branches whose upstream is gone are detected. Run as a tea.Cmd to keep
// the UI responsive while the (network-bound) fetch runs.
func fetchPruneCmd() tea.Cmd {
	return func() tea.Msg {
		_, err := runGit("fetch", "--all", "--prune")
		return fetchDoneMsg{err: err}
	}
}

// switchDoneMsg reports completion of an async `git switch`, carrying the
// reloaded repo so the reads run off the update loop too.
type switchDoneMsg struct {
	name     string
	err      error // the switch itself failed; the repo is untouched
	branches []branch
	reads    repoReads
	loadErr  error
}

// switchCmd checks out the named branch and reloads the repo, off the update
// loop like every other mutation (see fetchPruneCmd). `git switch` resolves
// branch names only, so a same-named tag cannot shadow it (see branchRef).
func switchCmd(name string) tea.Cmd {
	return func() tea.Msg {
		msg := switchDoneMsg{name: name}
		if _, msg.err = runGit("switch", name); msg.err != nil {
			return msg
		}
		msg.branches, msg.reads, msg.loadErr = loadRepo()
		return msg
	}
}

// branchDeletedMsg reports the outcome of one branch's async deletion; idx is
// its position in m.results.
type branchDeletedMsg struct {
	idx int
	res deleteResult
}

// spinnerTickMsg advances the deleting-view spinner animation.
type spinnerTickMsg struct{}

// spinnerFrames are the braille frames cycled while deletions are in flight.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// deleteBranchCmd wraps the deleteBranch worker as a tea.Cmd so deletions run off
// the update loop. It captures only a branch value (never the model), so each runs
// independently and concurrently under tea.Batch.
func deleteBranchCmd(idx int, b branch, flag string, wantRemote bool) tea.Cmd {
	return func() tea.Msg {
		return branchDeletedMsg{idx: idx, res: deleteBranch(b, flag, wantRemote)}
	}
}

// spinnerTickCmd schedules the next spinner frame.
func spinnerTickCmd() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return spinnerTickMsg{} })
}

// loadDiff returns the patch introduced on name relative to its merge-base with
// the repo's default branch — i.e. what the branch contains — and the base it was
// compared against, shortened for display.
func loadDiff(name string) (diff, base string, err error) {
	ref := baseBranch(name)
	if ref == "" {
		ref = "HEAD"
	}
	diff, err = runGit("diff", ref+"..."+branchRef(name))
	return diff, shortRef(ref), err
}

// ---- model ----

func initialModel() (model, error) {
	// The repo check runs beside the reads rather than ahead of them. It is here
	// to give a clearer message than git's own, not to gate the work, and a
	// serial subprocess start is most of what startup costs.
	var repoErr error
	checked := make(chan struct{})
	go func() {
		_, repoErr = runGit("rev-parse", "--is-inside-work-tree")
		close(checked)
	}()
	branches, reads, err := loadRepo()
	<-checked

	if repoErr != nil {
		return model{}, fmt.Errorf("not a git repository (or git is unavailable)")
	}
	if err != nil {
		return model{}, err
	}
	m := model{field: sortDate, ascending: false, height: 24, width: 100}
	m.applyBranches(branches, reads)
	return m, nil
}

func (m *model) sortBranches() {
	current := ""
	if b := m.cur(); b != nil {
		current = b.name
	}
	less := func(i, j int) bool {
		a, b := m.branches[i], m.branches[j]
		var r bool
		switch m.field {
		case sortName:
			r = a.name < b.name
		case sortAheadBehind:
			r = (a.ahead - a.behind) < (b.ahead - b.behind)
		default:
			r = a.committed.Before(b.committed)
		}
		if !m.ascending {
			return !r
		}
		return r
	}
	sort.SliceStable(m.branches, less)
	m.focusBranch(current)
	m.clampCursor()
}

// focusBranch puts the cursor on the named branch's view row. A filtered-out
// or unknown name leaves the cursor where it is. The cursor is a view
// position, not a branch index — this is the one place that mapping is done.
func (m *model) focusBranch(name string) {
	for p, i := range m.viewIdx() {
		if m.branches[i].name == name {
			m.cursor = p
			m.adjustScroll()
			return
		}
	}
}

func (m *model) clampCursor() {
	m.cursor = max(0, min(m.cursor, len(m.viewIdx())-1))
	m.adjustScroll()
}

// scroll applies a mouse-wheel step (delta of -1 up / +1 down) to whichever
// scrollable view is active; other states ignore the wheel.
func (m *model) scroll(delta int) {
	switch m.state {
	case stateList, stateFilter:
		m.cursor += delta
		m.clampCursor()
	case stateDiff:
		m.diffTop += delta * 3 // 3 lines per wheel notch, like a pager
		m.clampDiff()
	case stateConfirm, stateForcePrompt, stateDeleting, stateResult:
		m.bodyTop += delta * 3
		m.clampBody()
	}
}

func (m *model) visibleRows() int {
	return max(1, m.height-5) // minus header (2) + footer (3)
}

func (m *model) adjustScroll() {
	vis := m.visibleRows()
	if m.cursor < m.top {
		m.top = m.cursor
	}
	if m.cursor >= m.top+vis {
		m.top = m.cursor - vis + 1
	}
	m.top = max(0, m.top)
}

// viewIdx returns the indices of the branches the list shows, in display
// order. The cursor and scroll offsets are positions in this view, not in
// m.branches: marks live on the branches, so hiding a row must not move them.
func (m model) viewIdx() []int {
	idx := make([]int, 0, len(m.branches))
	f := strings.ToLower(m.filter)
	for i, b := range m.branches {
		if f == "" || strings.Contains(strings.ToLower(b.name), f) {
			idx = append(idx, i)
		}
	}
	return idx
}

func (m *model) cur() *branch {
	if idx := m.viewIdx(); m.cursor >= 0 && m.cursor < len(idx) {
		return &m.branches[idx[m.cursor]]
	}
	return nil
}

func (m model) selectedBranches() []branch {
	var out []branch
	for _, b := range m.branches {
		if b.selected {
			out = append(out, b)
		}
	}
	return out
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.adjustScroll()
		return m, nil
	case fetchDoneMsg:
		m.fetching = false
		if msg.err != nil {
			m.err = msg.err.Error()
			m.status = ""
			return m, nil
		}
		if branches, reads, err := loadRepo(); err == nil {
			// A fetch is non-destructive, so both the cursor (by name, in
			// sortBranches) and the user's pending marks survive it.
			m.applyBranches(carryMarks(m.branches, branches), reads)
		}
		// Auto-select only gone branches that carry nothing missing from the
		// base. Ones holding unique commits are left unselected so discarding
		// them stays a deliberate keystroke rather than a side effect of `p`.
		gone, risky := 0, 0
		for i := range m.branches {
			br := &m.branches[i]
			if !br.gone || br.isCurrent {
				continue
			}
			gone++
			if br.riskCommits > 0 {
				risky++
				continue
			}
			br.selected = true
		}
		m.err = ""
		switch {
		case gone == 0:
			m.status = "fetched & pruned — no gone branches"
		case risky == 0:
			m.status = fmt.Sprintf("fetched & pruned — %d gone branch(es) selected; press d to prune", gone)
		default:
			m.status = fmt.Sprintf("fetched & pruned — %d of %d gone branch(es) selected; %d hold commits not in %s (select with space to discard)",
				gone-risky, gone, risky, m.riskBase)
		}
		return m, nil
	case switchDoneMsg:
		m.switching = false
		if msg.err != nil {
			// A refused switch (dirty worktree, held by another worktree)
			// leaves the repo untouched; surface git's reason and stay put.
			m.err = msg.err.Error()
			m.status = ""
			return m, nil
		}
		m.err = ""
		m.status = "switched to " + msg.name
		if msg.loadErr == nil {
			// A switch deletes nothing, so pending marks survive it; carryMarks
			// drops the ones now on the current branch.
			m.applyBranches(carryMarks(m.branches, msg.branches), msg.reads)
		}
		m.focusBranch(msg.name)
		return m, nil
	case branchDeletedMsg:
		if msg.idx >= 0 && msg.idx < len(m.results) {
			m.results[msg.idx] = msg.res
		}
		if m.deletesDone() >= len(m.results) {
			m.reloadBranches()
			m.bodyTop = 0
			if len(m.forceableFailures()) > 0 {
				m.state = stateForcePrompt
			} else {
				m.state = stateResult
			}
		}
		return m, nil
	case spinnerTickMsg:
		if m.state == stateDeleting {
			m.spinnerFrame++
			return m, spinnerTickCmd()
		}
		return m, nil
	case tea.MouseMsg:
		// Handle the wheel ourselves so it scrolls the active view rather than
		// the terminal translating it into arrow-key bursts that leak between views.
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			m.scroll(-1)
		case tea.MouseButtonWheelDown:
			m.scroll(1)
		}
		return m, nil
	case tea.KeyMsg:
		switch m.state {
		case stateList:
			return m.updateList(msg)
		case stateFilter:
			return m.updateFilter(msg)
		case stateConfirm:
			return m.updateConfirm(msg)
		case stateForcePrompt:
			return m.updateForcePrompt(msg)
		case stateDiff:
			return m.updateDiff(msg)
		case stateDeleting:
			// Deletions are in flight; ignore input except an abort.
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
		case stateResult:
			switch msg.String() {
			case "q", "ctrl+c", "enter", "esc":
				return m, tea.Quit
			default:
				m.scrollKeys(msg.String())
			}
		case stateHelp:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			m.state = stateList
		}
	}
	return m, nil
}

// updateFilter handles keys in stateFilter, where the filter line captures
// input: plain letters (q, j, a, …) type into the filter instead of firing
// their list actions.
func (m model) updateFilter(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		m.state = stateList
	case "esc":
		m.filter = ""
		m.state = stateList
	case "backspace":
		if r := []rune(m.filter); len(r) > 0 {
			m.filter = string(r[:len(r)-1])
		}
	default:
		if msg.Type == tea.KeyRunes {
			m.filter += string(msg.Runes)
			m.cursor = 0
		}
	}
	m.clampCursor()
	return m, nil
}

func (m model) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		m.cursor--
		m.clampCursor()
	case "down", "j":
		m.cursor++
		m.clampCursor()
	case "g", "home":
		m.cursor = 0
		m.clampCursor()
	case "G", "end":
		m.cursor = len(m.branches) // clampCursor lands it on the last visible row
		m.clampCursor()
	case " ":
		if b := m.cur(); b != nil && !b.isCurrent {
			b.selected = !b.selected
		}
	case "r":
		if b := m.cur(); b != nil && b.upstream != "" && !b.isCurrent {
			b.deleteRemote = !b.deleteRemote
		}
	case "/":
		m.state = stateFilter
	case "esc":
		// n already disarms everything; esc only lifts the filter.
		m.filter = ""
		m.clampCursor()
	case "a":
		// Select what the list shows: with a filter active, a marks only the
		// matching branches, which is what makes filter-then-select useful.
		for _, i := range m.viewIdx() {
			if !m.branches[i].isCurrent {
				m.branches[i].selected = true
			}
		}
	case "n":
		for i := range m.branches {
			m.branches[i].selected = false
			m.branches[i].deleteRemote = false
		}
	case "s":
		m.field = (m.field + 1) % sortFieldCount
		m.sortBranches()
	case "o":
		m.ascending = !m.ascending
		m.sortBranches()
	case "f":
		m.force = !m.force
	case "p":
		if !m.fetching {
			m.fetching = true
			m.err = ""
			m.status = "fetching --all --prune…"
			return m, fetchPruneCmd()
		}
	case "v":
		if b := m.cur(); b != nil {
			diff, base, err := loadDiff(b.name)
			if err != nil {
				m.err = err.Error()
				break
			}
			m.err = ""
			m.diffBranch = b.name
			m.diffBase = base
			if strings.TrimSpace(diff) == "" {
				m.diffLines = nil
			} else {
				m.diffLines = strings.Split(strings.TrimRight(diff, "\n"), "\n")
			}
			m.diffTop = 0
			m.state = stateDiff
		}
	case "b":
		if b := m.cur(); b != nil && !b.isCurrent && !m.switching && !m.fetching {
			m.switching = true
			m.err = ""
			m.status = "switching to " + b.name + "…"
			return m, switchCmd(b.name)
		}
	case "?":
		m.state = stateHelp
	case "d", "enter":
		if len(m.selectedBranches()) > 0 {
			m.measureSelectedRisk() // the confirm screen states what each delete costs
			m.state, m.bodyTop = stateConfirm, 0
		}
	}
	return m, nil
}

func (m model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		// Local only: never delete remotes on the same key that deletes locals.
		return m, m.startDeletions(false)
	case "R":
		// Local + remote — only meaningful when at least one remote is armed.
		if m.armedRemoteCount() > 0 {
			return m, m.startDeletions(true)
		}
	case "n", "N", "esc", "q":
		m.state = stateList
	case "ctrl+c":
		return m, tea.Quit
	default:
		m.scrollKeys(msg.String())
	}
	return m, nil
}

// countArmedRemotes counts branches whose remote deletion is armed.
func countArmedRemotes(branches []branch) int {
	n := 0
	for _, b := range branches {
		if b.deleteRemote && b.upstream != "" {
			n++
		}
	}
	return n
}

// armedRemoteCount counts selected branches whose remote deletion is armed.
func (m model) armedRemoteCount() int {
	return countArmedRemotes(m.selectedBranches())
}

// startDeletions kicks off the asynchronous deletion of the selected branches,
// pre-seeding results, entering stateDeleting, and returning a batch of one cmd
// per branch (which run concurrently) plus the spinner tick. Remote branches are
// pushed --delete only when includeRemote is set (see updateConfirm).
func (m *model) startDeletions(includeRemote bool) tea.Cmd {
	m.measureSelectedRisk() // results carry the cost through to the force prompt
	sel := m.selectedBranches()
	m.results = make([]deleteResult, len(sel))
	m.spinnerFrame = 0
	m.state, m.bodyTop = stateDeleting, 0

	cmds := []tea.Cmd{spinnerTickCmd()}
	for i, b := range sel {
		m.results[i] = deleteResult{br: b}
		wantRemote := includeRemote && b.deleteRemote && b.upstream != ""
		cmds = append(cmds, deleteBranchCmd(i, b, b.deleteFlag(m.force), wantRemote))
	}
	return tea.Batch(cmds...)
}

// deletesDone counts how many of the current run's deletions have completed.
func (m model) deletesDone() int {
	n := 0
	for _, r := range m.results {
		if r.done {
			n++
		}
	}
	return n
}

// forceableFailures returns the results whose safe (-d) local delete was
// refused — the ones a force (-D) delete could clear.
func (m model) forceableFailures() []deleteResult {
	var out []deleteResult
	for _, r := range m.results {
		if !r.localOK && r.forceable {
			out = append(out, r)
		}
	}
	return out
}

func (m model) updateForcePrompt(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		m.forceDeleteUnmerged()
		m.state, m.bodyTop = stateResult, 0
	case "n", "N", "esc", "q", "enter":
		m.state, m.bodyTop = stateResult, 0
	case "ctrl+c":
		return m, tea.Quit
	default:
		m.scrollKeys(msg.String())
	}
	return m, nil
}

func (m *model) clampDiff() {
	m.diffTop = max(0, min(m.diffTop, len(m.diffLines)-m.visibleRows()))
}

func (m model) updateDiff(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc", "v":
		m.state = stateList
	case "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		m.diffTop--
		m.clampDiff()
	case "down", "j":
		m.diffTop++
		m.clampDiff()
	case "ctrl+u", "pgup":
		m.diffTop -= m.visibleRows() / 2
		m.clampDiff()
	case "ctrl+d", "pgdown", " ":
		m.diffTop += m.visibleRows() / 2
		m.clampDiff()
	case "g", "home":
		m.diffTop = 0
	case "G", "end":
		m.diffTop = len(m.diffLines)
		m.clampDiff()
	}
	return m, nil
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
// update loop, one cmd per branch.
func deleteBranch(b branch, flag string, wantRemote bool) deleteResult {
	res := deleteResult{br: b, done: true}
	if _, err := runGit("branch", flag, b.name); err != nil {
		res.localErr = err.Error()
		// Only an unmerged refusal is worth escalating to -D. Other failures — a
		// branch held by another worktree, most commonly — fail identically under
		// -D, so offering the retry would just mislabel them as lost commits.
		res.forceable = flag == "-d" && strings.Contains(res.localErr, "not fully merged")
	} else {
		res.localOK = true
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

// carryMarks copies the user's pending selections from old onto a freshly loaded
// branch set, matching by name. Used on the fetch path, which reloads every
// branch struct but changes nothing the marks were made about. An armed remote
// delete is dropped when the fetch reveals the upstream is already gone: the push
// it would run can only fail.
func carryMarks(old, fresh []branch) []branch {
	prev := make(map[string]branch, len(old))
	for _, b := range old {
		prev[b.name] = b
	}
	for i := range fresh {
		if p, ok := prev[fresh[i].name]; ok {
			// Marks never land on the current branch: it cannot be deleted, so
			// a carried mark would arm an operation the list refuses to offer
			// (relevant after a switch, or when HEAD moved outside the TUI).
			fresh[i].selected = p.selected && !fresh[i].isCurrent
			fresh[i].deleteRemote = p.deleteRemote && !fresh[i].gone && !fresh[i].isCurrent
		}
	}
	return fresh
}

// applyBranches installs a freshly-loaded branch set and recomputes everything
// derived from it (name-width, merge info, sort order). Callers set their own
// cursor policy around it. This is the single refresh core shared by
// reloadBranches and the fetch handler.
func (m *model) applyBranches(branches []branch, reads repoReads) {
	m.branches = branches
	m.recomputeNameWidth()
	m.refreshMergeInfo(reads)
	m.sortBranches()
}

// reloadBranches refreshes the branch list from git and resets the view to the
// top — appropriate after a mutation that may have removed the cursor's branch.
func (m *model) reloadBranches() {
	if branches, reads, err := loadRepo(); err == nil {
		m.cursor = 0
		m.top = 0
		m.applyBranches(branches, reads)
	}
}

// hasBranch reports whether the loaded list holds a local branch of that name.
func (m model) hasBranch(name string) bool {
	for _, b := range m.branches {
		if b.name == name {
			return true
		}
	}
	return false
}

// refreshMergeInfo caches the remote default branch, marks each branch whose
// upstream is merged into it or whose tip is merged into HEAD, and measures what
// deleting it would cost. Call after every branch (re)load, passing the reads
// that came back with it.
func (m *model) refreshMergeInfo(reads repoReads) {
	defRef := remoteDefaultFrom(reads.remotes)

	m.riskBaseRef = defRef
	if m.riskBaseRef == "" {
		// The branch list is already loaded, so this candidate check is a lookup
		// rather than a subprocess per name.
		m.riskBaseRef = localDefaultBranch("", m.hasBranch)
	}
	// The refs drive git; the short forms are what the views print.
	m.remoteDefault = shortRef(defRef)
	m.riskBase = shortRef(m.riskBaseRef)

	// Round two. Neither query needs the other's answer, and each is a
	// subprocess start, so they go together.
	var merged map[string]bool
	remote := make(chan struct{})
	go func() {
		merged = remoteMergedSet(defRef)
		close(remote)
	}()
	// One query answers "is this branch's tip already in the base?" for the whole
	// list, which is the answer for most branches a prune touches.
	m.baseMerged = nil
	if m.riskBaseRef != "" {
		m.baseMerged = mergedSet("branch", "--merged", m.riskBaseRef)
	}
	<-remote

	for i := range m.branches {
		b := &m.branches[i]
		b.remoteMerged = b.upstream != "" && merged[b.upstream]
		b.headMerged = reads.headMerged[b.name]
		b.riskMeasured = false // the branch was just reloaded; any old count is stale
	}
	// Only gone branches are measured up front, because `p` consults the count to
	// decide what it may auto-select. The rest wait for measureSelectedRisk:
	// riskCommitCount is a subprocess per branch, and measuring every unmergeable
	// branch here would put a network-free repo's whole branch list on the clock.
	m.measureRisk(func(b branch) bool { return b.gone })
}

// measureRisk fills in the cost-of-deletion count for every not-yet-measured
// branch that want accepts. The counts are independent `git cherry` subprocesses
// whose cost is dominated by process spawn, so they run concurrently: measured
// one at a time, a repo with a hundred gone branches spent 1.1s here on every
// load, fetch and prune.
func (m *model) measureRisk(want func(branch) bool) {
	base := m.riskBaseRef // read once: the goroutines must not touch the model
	var wg sync.WaitGroup
	for i := range m.branches {
		if m.branches[i].riskMeasured || !want(m.branches[i]) {
			continue
		}
		// A branch already contained in the base has an empty base..branch range,
		// so `git cherry` would report nothing. Answer from the set instead of
		// spawning the subprocess.
		if m.baseMerged[m.branches[i].name] {
			m.branches[i].riskCommits = 0
			m.branches[i].riskMeasured = true
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each goroutine owns one slice element, so no two write the same branch.
			m.branches[i].riskCommits = riskCommitCount(m.branches[i].name, base)
			m.branches[i].riskMeasured = true
		}()
	}
	wg.Wait()
}

// measureSelectedRisk measures what deleting each selected branch would cost.
// Call before any view that reports the cost: only branches a safe delete would
// refuse are measured, since those are the ones deleted with -D.
func (m *model) measureSelectedRisk() {
	m.measureRisk(func(b branch) bool { return b.selected && (b.gone || !b.safeDeletable()) })
}

// forceDeleteUnmerged re-runs the deletions that a safe (-d) delete refused,
// this time with -D. It updates the matching result in place so the results
// screen reflects the retry outcome.
func (m *model) forceDeleteUnmerged() {
	for i := range m.results {
		r := &m.results[i]
		if r.localOK || !r.forceable {
			continue
		}
		if _, err := runGit("branch", "-D", r.br.name); err != nil {
			r.localErr = err.Error()
			continue
		}
		r.localOK = true
		r.localErr = ""
		// The armed remote delete was deferred while the local branch survived;
		// now that it is gone, honour what the user confirmed.
		if r.remoteSkipped {
			pushRemoteDelete(r)
		}
	}
	m.reloadBranches()
}

// ---- views ----

func (m model) View() string {
	switch m.state {
	case stateConfirm:
		return m.confirmView()
	case stateForcePrompt:
		return m.forcePromptView()
	case stateDeleting:
		return m.deletingView()
	case stateResult:
		return m.resultView()
	case stateHelp:
		return m.helpView()
	case stateDiff:
		return m.diffView()
	default:
		return m.listView()
	}
}

// page renders a screen as fixed header lines, a window into body, and fixed
// footer lines. The prompt on these screens is the whole point of them, and it
// lives in the footer: without a window, a wide selection pushes the question
// past the last row of the terminal, where the user cannot read what they are
// answering. Rendering only the visible rows is also what keeps a long list off
// the cost of every frame.
func (m model) page(header, body, footer []string) string {
	rows := m.bodyRows(len(header), len(footer), len(body))
	top := max(0, min(m.bodyTop, len(body)-rows))
	end := min(top+rows, len(body))

	var b strings.Builder
	for _, l := range header {
		b.WriteString(l + "\n")
	}
	for _, l := range body[top:end] {
		b.WriteString(l + "\n")
	}
	if len(body) > rows {
		b.WriteString(dimStyle.Render(fmt.Sprintf("[%d-%d / %d]  ↑/↓ scroll · space/ctrl+d page · g/G top/bottom",
			top+1, end, len(body))) + "\n")
	}
	for _, l := range footer {
		b.WriteString(l + "\n")
	}
	return b.String()
}

// bodyRows is how many body lines fit between header and footer. A body that
// does not fit gives up one more row to the position line, so that line never
// pushes the footer off in its turn.
func (m model) bodyRows(header, footer, body int) int {
	rows := max(1, m.height-header-footer)
	if body > rows {
		rows = max(1, rows-1)
	}
	return rows
}

// pageParts returns the current state's screen as header, body and footer. The
// scroll keys measure against it too, so the window and the clamp can never
// disagree about how far down the body goes.
func (m model) pageParts() (header, body, footer []string) {
	switch m.state {
	case stateConfirm:
		return m.confirmParts()
	case stateForcePrompt:
		return m.forcePromptParts()
	case stateDeleting:
		return m.deletingParts()
	case stateResult:
		return m.resultParts()
	}
	return nil, nil, nil
}

// bodyWindow reports the visible row count and the total body length for the
// current state.
func (m model) bodyWindow() (rows, total int) {
	header, body, footer := m.pageParts()
	return m.bodyRows(len(header), len(footer), len(body)), len(body)
}

func (m *model) clampBody() {
	rows, total := m.bodyWindow()
	m.bodyTop = max(0, min(m.bodyTop, total-rows))
}

// scrollKeys applies the shared paging keys to a windowed screen. It reports
// whether the key was one of them, so each screen's own keys stay in charge:
// callers must offer their answers first.
func (m *model) scrollKeys(s string) bool {
	rows, total := m.bodyWindow()
	switch s {
	case "up", "k":
		m.bodyTop--
	case "down", "j":
		m.bodyTop++
	case "ctrl+u", "pgup":
		m.bodyTop -= max(1, rows/2)
	case "ctrl+d", "pgdown", " ":
		m.bodyTop += max(1, rows/2)
	case "g", "home":
		m.bodyTop = 0
	case "G", "end":
		m.bodyTop = total
	default:
		return false
	}
	m.clampBody()
	return true
}

func colorizeDiffLine(line string) string {
	switch {
	case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		return headerStyle.Render(line)
	case strings.HasPrefix(line, "diff "), strings.HasPrefix(line, "index "),
		strings.HasPrefix(line, "new file"), strings.HasPrefix(line, "deleted file"),
		strings.HasPrefix(line, "rename "), strings.HasPrefix(line, "similarity "):
		return dimStyle.Render(line)
	case strings.HasPrefix(line, "@@"):
		return hunkStyle.Render(line)
	case strings.HasPrefix(line, "+"):
		return addStyle.Render(line)
	case strings.HasPrefix(line, "-"):
		return delStyle.Render(line)
	default:
		return line
	}
}

func (m model) diffView() string {
	var b strings.Builder

	base := m.diffBase
	b.WriteString(headerStyle.Render(fmt.Sprintf("diff — %s (vs %s)", m.diffBranch, base)))
	b.WriteString("\n\n")

	if len(m.diffLines) == 0 {
		b.WriteString(dimStyle.Render("no changes — branch matches " + base))
		b.WriteString("\n\n")
		b.WriteString(dimStyle.Render("q/esc back · v back"))
		b.WriteString("\n")
		return b.String()
	}

	vis := m.visibleRows()
	end := min(m.diffTop+vis, len(m.diffLines))
	for i := m.diffTop; i < end; i++ {
		b.WriteString(colorizeDiffLine(truncate(m.diffLines[i], m.width)))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	pos := fmt.Sprintf("[%d-%d / %d]", m.diffTop+1, end, len(m.diffLines))
	help := "↑/↓ scroll · space/ctrl+d page · g/G top/bottom · q/esc/v back"
	b.WriteString(dimStyle.Render(pos + "  " + help))
	b.WriteString("\n")
	return b.String()
}

// recomputeNameWidth caches the branch-name column width; call whenever the
// branch list changes (it depends only on the set of names, not render state).
func (m *model) recomputeNameWidth() {
	w := 0
	for _, br := range m.branches {
		w = max(w, ansi.StringWidth(br.name))
	}
	m.nameW = min(40, max(6, w))
}

func (m model) listView() string {
	var b strings.Builder

	dir := "desc"
	if m.ascending {
		dir = "asc"
	}
	forceLabel := "safe (-d)"
	if m.force {
		forceLabel = "FORCE (-D)"
	}
	idx := m.viewIdx()
	header := fmt.Sprintf("git_pruner — %d branches   sort: %s %s   delete mode: %s",
		len(m.branches), m.field, dir, forceLabel)
	b.WriteString(headerStyle.Render(header))
	if m.filter != "" || m.state == stateFilter {
		f := m.filter
		if m.state == stateFilter {
			f += "▌"
		}
		b.WriteString(selStyle.Render(fmt.Sprintf("   filter: %s (%d/%d)", f, len(idx), len(m.branches))))
	}
	b.WriteString("\n\n")

	switch {
	case len(m.branches) == 0:
		b.WriteString(dimStyle.Render("no local branches found"))
		b.WriteString("\n")
	case len(idx) == 0:
		b.WriteString(dimStyle.Render("no branches match filter"))
		b.WriteString("\n")
	}

	nameW := m.nameW
	vis := m.visibleRows()
	end := min(m.top+vis, len(idx))
	for p := m.top; p < end; p++ {
		b.WriteString(m.renderRow(m.branches[idx[p]], nameW, p == m.cursor))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	var help string
	if m.state == stateFilter {
		help = "type to filter · enter keep · esc clear · backspace edit"
	} else {
		help = "↑/↓ move · space select · a/n all/none · b switch · / filter · r remote · v view · p prune · s sort · o order · f force · d delete · ? help · q quit"
	}
	b.WriteString(dimStyle.Render(help))
	if m.status != "" {
		b.WriteString("\n")
		b.WriteString(okStyle.Render(m.status))
	}
	if m.err != "" {
		b.WriteString("\n")
		b.WriteString(errStyle.Render(m.err))
	}
	return b.String()
}

func (m model) renderRow(br branch, nameW int, isCursor bool) string {
	cursor := "  "
	if isCursor {
		cursor = cursorStyle.Render("> ")
	}
	sel := "[ ]"
	if br.selected {
		sel = selStyle.Render("[x]")
	}
	rem := " "
	if br.deleteRemote {
		rem = errStyle.Render("R")
	}
	cur := " "
	if br.isCurrent {
		cur = currentStyle.Render("*")
	}

	name := pad(truncate(br.name, nameW), nameW)

	var nameRendered string
	switch {
	case isCursor:
		nameRendered = cursorStyle.Render(name)
	case br.isCurrent:
		nameRendered = currentStyle.Render(name)
	case br.selected:
		nameRendered = selStyle.Render(name)
	default:
		nameRendered = nameStyle.Render(name)
	}

	track := m.trackStr(br)
	abs := fmt.Sprintf("%-11s", br.committed.Format("2006-Jan-02"))
	rel := fmt.Sprintf("%-13s", br.committedRel)
	hash := fmt.Sprintf("%-8s", br.hash)

	return fmt.Sprintf("%s%s %s %s %s  %s %s %s %s %s",
		cursor, sel, rem, cur, nameRendered, track,
		dimStyle.Render(abs), dimStyle.Render(rel), hashStyle.Render(hash),
		subjectStyle.Render(truncate(br.subject, m.subjectWidth(nameW))))
}

func (m model) trackStr(br branch) string {
	if br.gone {
		// Same column style as every other track value, or the columns that
		// follow shift left on exactly the rows the user is here to act on.
		return trackColStyle.Render(goneStyle.Render("gone"))
	}
	if br.upstream == "" {
		return trackColStyle.Render(dimStyle.Render("-"))
	}
	s := ""
	if br.ahead > 0 {
		s += aheadStyle.Render("↑" + strconv.Itoa(br.ahead))
	}
	if br.behind > 0 {
		s += behindStyle.Render("↓" + strconv.Itoa(br.behind))
	}
	if s == "" {
		s = currentStyle.Render("=")
	}
	if br.remoteMerged { // upstream is merged into the remote default — safe to delete
		s += okStyle.Render(" ✓")
	}
	return trackColStyle.Render(s)
}

func (m model) subjectWidth(nameW int) int {
	// Sum of every fixed column width and separator in renderRow's format,
	// plus nameW; keep in sync with that format string. The 10 is the track
	// column (trackColStyle width); the trailing 8 is the hash column.
	used := 2 + 3 + 1 + 1 + 1 + 1 + 1 + 1 + nameW + 2 + 10 + 1 + 11 + 1 + 13 + 1 + 8 + 1
	return max(10, m.width-used)
}

// truncate shortens s to w terminal cells, appending an ellipsis when it does
// not fit. Measured in display cells rather than bytes so multibyte text is
// never sliced mid-rune and wide (CJK/emoji) characters do not overflow.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "…")
}

// pad right-pads s to w display cells. The fmt width verbs count runes, which
// misaligns columns whose content contains wide characters.
func pad(s string, w int) string {
	if d := w - ansi.StringWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

func (m model) helpView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("git_pruner — help"))
	b.WriteString("\n\n")

	writeRows := func(pairs [][2]string) {
		for _, p := range pairs {
			b.WriteString("  " + cursorStyle.Render(fmt.Sprintf("%-14s", p[0])) + subjectStyle.Render(p[1]) + "\n")
		}
	}

	writeRows([][2]string{
		{"↑/↓, j/k", "move cursor"},
		{"g/G, home/end", "jump to first/last"},
		{"space", "select / deselect branch"},
		{"a / n", "select all listed / none"},
		{"b", "switch to the branch under the cursor"},
		{"/", "filter by name (enter keep · esc clear)"},
		{"r", "toggle delete of upstream remote branch"},
		{"v", "view branch diff (green add / red remove)"},
		{"p", "fetch --all --prune & select safe gone branches"},
		{"s", "cycle sort field (date, name, ahead/behind)"},
		{"o", "toggle sort order (asc/desc)"},
		{"f", "toggle force delete (-d / -D)"},
		{"d, enter", "delete selected branches (local)"},
		{"", "on confirm: y = local only · R = local + remote"},
		{"", "(unmerged -d failures prompt to retry with -D)"},
		{"", "long lists scroll: ↑/↓ · space/ctrl+d · g/G"},
		{"?", "toggle this help screen"},
		{"q, ctrl+c", "quit"},
	})

	b.WriteString("\n")
	b.WriteString(headerStyle.Render("Columns"))
	b.WriteString("\n")
	writeRows([][2]string{
		{"*", "current branch (cannot be deleted)"},
		{"[x]", "selected for deletion"},
		{"R", "its remote branch will also be deleted"},
		{"↑/↓", "commits ahead of / behind upstream"},
		{"✓", "upstream merged into remote default (safe)"},
		{"gone", "upstream was configured but no longer exists"},
	})

	b.WriteString("\n")
	b.WriteString(dimStyle.Render("Gone branches are deleted with -D. Any holding commits that are not in\n" +
		"the default branch are left unselected by p and flagged on the confirm screen."))
	b.WriteString("\n")

	b.WriteString("\n")
	b.WriteString(dimStyle.Render("press any key to return"))
	b.WriteString("\n")
	return b.String()
}

func (m model) confirmParts() (header, body, footer []string) {
	sel := m.selectedBranches()
	flag := "-d (safe)"
	if m.force {
		flag = "-D (force)"
	}
	remoteCount := countArmedRemotes(sel)
	header = []string{
		headerStyle.Render("Confirm deletion"),
		"",
		fmt.Sprintf("Local delete mode: %s", flag),
		fmt.Sprintf("Deleting %d local branch(es), %d remote branch(es).", len(sel), remoteCount),
		"",
	}

	for _, br := range sel {
		body = append(body, "  "+cursorStyle.Render("• "+br.name))

		date := br.committed.Format("2006-Jan-02")
		if br.committedRel != "" {
			date += " (" + br.committedRel + ")"
		}
		body = append(body, "      "+dimStyle.Render(fmt.Sprintf("%s  %s  %s", br.hash, date, truncate(br.subject, 50))))

		switch {
		case br.gone:
			body = append(body, "      "+goneStyle.Render("upstream gone: "+br.upstream+" — will prune with -D (force)"))
		case br.upstream != "":
			body = append(body, "      "+dimStyle.Render("upstream: "+br.upstream)+" "+m.trackStr(br))
		default:
			body = append(body, "      "+dimStyle.Render("no upstream"))
		}

		if br.upstream != "" && m.remoteDefault != "" {
			if br.remoteMerged {
				body = append(body, "      "+okStyle.Render("✓ merged into "+m.remoteDefault))
			} else {
				body = append(body, "      "+goneStyle.Render("⚠ not merged into "+m.remoteDefault))
			}
		}

		if br.deleteRemote && br.upstream != "" {
			body = append(body, "      "+errStyle.Render(fmt.Sprintf("+ delete remote %s/%s", br.remoteName(), br.remoteBranch())))
		}
		if w := m.riskWarning(br); w != "" {
			body = append(body, "      "+errStyle.Render(w))
		}
		body = append(body, "")
	}

	prompt := headerStyle.Render("Delete these branches? ")
	if remoteCount > 0 {
		prompt += dimStyle.Render(fmt.Sprintf("(y = local only · R = local + remote (%d) · n/esc = cancel)", remoteCount))
	} else {
		prompt += dimStyle.Render("(y = yes · n/esc = cancel)")
	}
	return header, body, []string{prompt}
}

func (m model) confirmView() string { return m.page(m.confirmParts()) }

// riskWarning states the cost of deleting br, or "" when the delete is clean.
// It covers every branch git's safe delete would refuse plus gone branches,
// which take the -D path regardless: under -D the unmerged commits are
// discarded, under -d the delete simply fails.
func (m model) riskWarning(br branch) string {
	if br.safeDeletable() && !br.gone {
		return ""
	}
	if br.forcedDelete(m.force) { // -D: the question is what gets discarded
		if br.riskCommits > 0 {
			return fmt.Sprintf("⚠ %d commit(s) not in %s — force delete (-D) will discard them", br.riskCommits, m.riskBase)
		}
		if m.riskBase == "" {
			return "⚠ no base branch to compare against — force delete (-D) discards any unmerged commits"
		}
		return "" // measured against a real base: nothing here is at risk
	}
	// -d will be refused either way; the count is what a force would then cost.
	if br.riskCommits > 0 {
		return fmt.Sprintf("⚠ not fully merged: %d commit(s) not in %s — safe delete (-d) will fail; use force (f)", br.riskCommits, m.riskBase)
	}
	return "⚠ not fully merged — safe delete (-d) will fail; use force (f)"
}

func (m model) forcePromptParts() (header, body, footer []string) {
	failures := m.forceableFailures()

	header = []string{
		headerStyle.Render("Force delete unmerged branches?"),
		"",
		fmt.Sprintf("%d branch(es) were refused by safe delete (-d) because they are not", len(failures)),
		"fully merged. Force deleting (-D) will " + errStyle.Render("permanently discard their unmerged commits") + ".",
		"",
	}

	for _, r := range failures {
		body = append(body, "  "+cursorStyle.Render("• "+r.br.name))
		// Every branch here failed -d, so its risk was measured before the delete
		// ran: riskCommits == 0 means either nothing is missing from the base or
		// there was no base to measure against.
		switch {
		case r.br.riskCommits > 0:
			body = append(body, "      "+errStyle.Render(fmt.Sprintf("⚠ %d commit(s) not in %s will be lost", r.br.riskCommits, m.riskBase)))
		case m.riskBase == "":
			body = append(body, "      "+errStyle.Render("⚠ no base branch to compare against — unmerged commits may be lost"))
		default:
			body = append(body, "      "+dimStyle.Render("no commits missing from "+m.riskBase))
		}
		if r.remoteSkipped {
			body = append(body, "      "+errStyle.Render(fmt.Sprintf("+ remote %s/%s will be deleted once the branch is gone", r.br.remoteName(), r.br.remoteBranch())))
		}
	}

	footer = []string{
		"",
		headerStyle.Render("Force delete (-D) these branches? ") + dimStyle.Render("(y = yes, discard · n/esc = keep them)"),
	}
	return header, body, footer
}

func (m model) forcePromptView() string { return m.page(m.forcePromptParts()) }

// appendResultLines adds one completed deletion result (local, then remote if
// tried) to dst. Shared by the results screen and the live deleting screen.
func appendResultLines(dst []string, r deleteResult) []string {
	if r.localOK {
		dst = append(dst, okStyle.Render("  ✓ ")+"deleted local "+r.br.name)
	} else {
		dst = append(dst, errStyle.Render("  ✗ ")+"local "+r.br.name+": "+r.localErr)
	}
	switch {
	case r.remoteSkipped:
		// Say why the armed remote survived, or it reads as a silent failure.
		dst = append(dst, errStyle.Render("  ! ")+"kept remote "+r.br.remoteName()+"/"+r.br.remoteBranch()+": local delete failed")
	case r.remoteTried && r.remoteOK:
		dst = append(dst, okStyle.Render("  ✓ ")+"deleted remote "+r.br.name)
	case r.remoteTried:
		dst = append(dst, errStyle.Render("  ✗ ")+"remote "+r.br.name+": "+r.remoteErr)
	}
	return dst
}

func (m model) deletingParts() (header, body, footer []string) {
	spin := spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
	header = []string{
		headerStyle.Render(fmt.Sprintf("%s Deleting… (%d/%d)", spin, m.deletesDone(), len(m.results))),
		"",
	}
	for _, r := range m.results {
		if r.done {
			body = appendResultLines(body, r)
		} else {
			body = append(body, dimStyle.Render("  "+spin+" deleting "+r.br.name+"…"))
		}
	}
	return header, body, []string{"", dimStyle.Render("working — ctrl+c to abort")}
}

func (m model) deletingView() string { return m.page(m.deletingParts()) }

func (m model) resultParts() (header, body, footer []string) {
	header = []string{headerStyle.Render("Results"), ""}
	for _, r := range m.results {
		body = appendResultLines(body, r)
	}
	return header, body, []string{"", dimStyle.Render("press q/enter to quit")}
}

func (m model) resultView() string { return m.page(m.resultParts()) }

// versionString reports the build's commit and date using Go's automatic VCS
// stamping (populated when built with `go build` inside the repo). Fields fall
// back to "unknown" when build info is unavailable (e.g. `go run`).
func versionString() string {
	commit, date, goVer, dirty := "unknown", "unknown", "unknown", false
	if info, ok := debug.ReadBuildInfo(); ok {
		goVer = info.GoVersion
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				commit = s.Value
			case "vcs.time":
				date = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	if len(commit) > 7 { // shorten a real SHA; the "unknown" fallback is 7 chars
		commit = commit[:7]
	}
	if dirty {
		commit += " (dirty)"
	}
	return fmt.Sprintf("git_pruner\n  commit: %s\n  date:   %s\n  go:     %s", commit, date, goVer)
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println(versionString())
			return
		}
	}

	m, err := initialModel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "git_pruner:", err)
		os.Exit(1)
	}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "git_pruner:", err)
		os.Exit(1)
	}
}
