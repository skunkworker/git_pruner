package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

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
	diffRaw    string   // unified diff as git produced it; delta is re-run from it on resize
	diffLines  []string // display lines of the diff being viewed
	diffStyled bool     // lines came from delta and carry their own colors
	diffTop    int      // scroll offset within diffLines

	bodyTop int // scroll offset within the confirm/force/result body (see page)

	width, height int
	err           string
	status        string // transient info message (e.g. fetch results)
	fetching      bool   // a background fetch --all --prune is in flight
	switching     bool   // a background git switch is in flight
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
		if m.state == stateDiff && m.diffStyled {
			// delta laid its columns out for the old width
			m.diffLines, m.diffStyled = styleDiff(m.diffRaw, m.width)
		}
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
		m.err = ""
		m.status = "fetched & pruned — " + m.selectGone()
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
	case "x":
		// The same selection `p` makes after its fetch, for a repo whose
		// gone marks are already known; no network round trip needed.
		m.err = ""
		m.status = m.selectGone()
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
			m.diffRaw = diff
			m.diffLines, m.diffStyled = styleDiff(diff, m.width)
			m.diffTop = 0
			m.state = stateDiff
		}
	case "c":
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

// selectGone selects every gone branch that carries nothing missing from the
// base and reports the outcome for the status line. Branches holding unique
// commits stay unselected so discarding them is a deliberate keystroke rather
// than a side effect of `p` or `x`.
func (m *model) selectGone() string {
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
	switch {
	case gone == 0:
		return "no gone branches"
	case risky == 0:
		return fmt.Sprintf("%d gone branch(es) selected; press d to prune", gone)
	default:
		return fmt.Sprintf("%d of %d gone branch(es) selected; %d hold commits not in %s (select with space to discard)",
			gone-risky, gone, risky, m.riskBase)
	}
}
