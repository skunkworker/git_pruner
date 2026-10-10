package main

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strconv"
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
	stateAge    // the list, with the merged-and-old prompt capturing digits
)

type deleteResult struct {
	br   branch // the branch this deletion was run for
	done bool   // the async deletion for this branch has completed
	// worktreeRemoved records that the linked worktree holding br was removed
	// to free the branch for deletion.
	worktreeRemoved bool
	localOK         bool
	localErr        string
	forceable       bool // a safe (-d) delete failed and could be retried with -D
	remoteTried     bool
	remoteOK        bool
	remoteErr       string
	// remoteSkipped records that the armed push was deliberately deferred
	// because the local delete failed — the one piece of state not derivable
	// from br, since arming is the caller's decision.
	remoteSkipped bool
	restored      bool // undo recreated the local branch
}

// worktreePending reports whether a linked worktree still holds r's branch, so
// a force retry has to remove it first.
func (r deleteResult) worktreePending() bool { return r.br.worktree != "" && !r.worktreeRemoved }

// restorable reports whether undo can bring r's local branch back.
func (r deleteResult) restorable() bool {
	return r.localOK && !r.restored && r.br.sha != ""
}

type model struct {
	branches []branch // local rows and, once loaded, remoteOnly rows
	cursor   int
	top      int // index of first visible row (scroll window)

	field     sortField
	ascending bool
	force     bool
	nameW     int // cached branch-name column width (see recomputeNameWidth)

	filter     string // active branch-name filter; "" shows every branch
	ageInput   string // digits typed into the merged-and-old prompt
	showRemote bool   // the list shows remoteOnly rows instead of local ones
	// remoteLoaded records that the remote rows were asked for once; every
	// later reload then keeps them current.
	remoteLoaded bool

	state   viewState
	results []deleteResult
	history []deleteResult // earlier runs this session, for the exit summary
	// abortArmed is set by a first ctrl+c while deletions run. Quitting then
	// can strand a remote copy, so a second press is needed.
	abortArmed bool

	config           repoConfig
	remoteDefault    string // resolved remote default branch, e.g. "origin/main"
	remoteDefaultRef string // remoteDefault fully qualified
	riskBase         string // ref that branch.riskCommits is measured against ("" if unresolved)
	riskBaseRef      string // riskBase fully qualified, so a same-named tag cannot shadow it
	// baseMerged holds the local branches whose tip is an ancestor of riskBaseRef.
	// Their riskCommits is 0 by definition, so one query here removes a `git
	// cherry` subprocess per branch (see measureRisk).
	baseMerged map[string]bool
	// merging is set while the startup merge queries run in the background.
	// Until they land, ✓ marks and risk counts are unknown, so the keys that
	// select by them wait.
	merging bool
	loadGen int // bumped per branch load, so a late merge result for an old list is dropped

	spinnerFrame int // animation frame for the deleting spinner (deletion counts derive from results)

	diffBranch  string   // branch whose diff is shown in stateDiff
	diffBase    string   // base ref the diff was computed against
	diffRaw     string   // unified diff as git produced it; delta is re-run from it on resize
	diffLines   []string // display lines of the diff being viewed
	diffStyled  bool     // lines came from delta and carry their own colors
	diffTop     int      // scroll offset within diffLines
	diffLoading bool     // the diff (or its re-layout) is being made in the background
	diffSeq     int      // bumped per diff request, so a slow answer for an old one is dropped

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
	name       string // the local branch now checked out
	fromRemote bool   // the switch created name from a remoteOnly row
	err        error  // the switch itself failed; the repo is untouched
	branches   []branch
	reads      repoReads
	loadErr    error
}

// switchCmd checks out b and reloads the repo, off the update loop like every
// other mutation (see fetchPruneCmd). `git switch` resolves branch names only,
// so a same-named tag cannot shadow it (see branchRef). A remoteOnly row gets a
// new local branch that tracks it; the start point is the full ref, because a
// bare "origin/x" would lose to a tag of that name.
func switchCmd(b branch, withRemote bool) tea.Cmd {
	args := []string{"switch", b.name}
	msg := switchDoneMsg{name: b.name}
	if b.remoteOnly {
		// Name the local copy after the tracking ref, as git's own checkout
		// does: a fetch refspec can give the remote's branch another name.
		_, local, _ := strings.Cut(b.name, "/")
		msg.name, msg.fromRemote = local, true
		args = []string{"switch", "-c", msg.name, "--track", b.ref()}
	}
	return func() tea.Msg {
		if _, msg.err = runGit(args...); msg.err != nil {
			return msg
		}
		msg.branches, msg.reads, msg.loadErr = loadRepo(withRemote)
		return msg
	}
}

// repoLoadedMsg carries a background reload (see reloadCmd).
type repoLoadedMsg struct {
	branches []branch
	reads    repoReads
	err      error
}

// reloadCmd reloads the repo off the update loop. The first switch to the
// remote view uses it: reading every remote ref's commit can be slow on a big
// remote.
func reloadCmd(withRemote bool) tea.Cmd {
	return func() tea.Msg {
		b, r, err := loadRepo(withRemote)
		return repoLoadedMsg{b, r, err}
	}
}

// mergeInfo is the result of the merge queries (see queryMergeInfo).
type mergeInfo struct {
	remoteMerged map[string]bool // remote-tracking names merged into the remote default
	baseMerged   map[string]bool // local branches whose tip is in the risk base
	risk         map[string]int  // commits not in the base, by ref, for gone branches
}

// mergeInfoMsg carries the startup merge queries; gen ties it to one load.
type mergeInfoMsg struct {
	gen  int
	info mergeInfo
}

// diffLoadedMsg carries a diff made in the background; seq ties it to one request.
type diffLoadedMsg struct {
	seq    int
	base   string // "" on a re-layout: the base is unchanged
	raw    string
	lines  []string
	styled bool
	err    error
}

// diffCmd reads b's diff and lays it out, off the update loop: a big diff
// through delta takes long enough to freeze the screen.
func diffCmd(seq int, b branch, width int) tea.Cmd {
	return func() tea.Msg {
		msg := diffLoadedMsg{seq: seq}
		msg.raw, msg.base, msg.err = loadDiffRef(b.ref(), b.name)
		if msg.err == nil {
			msg.lines, msg.styled = styleDiff(msg.raw, width)
		}
		return msg
	}
}

// restyleTickMsg fires a moment after a resize (see the WindowSizeMsg case).
type restyleTickMsg struct{ seq int }

// restyleCmd re-runs delta on a diff already read, for a new terminal width.
func restyleCmd(seq int, raw string, width int) tea.Cmd {
	return func() tea.Msg {
		msg := diffLoadedMsg{seq: seq, raw: raw}
		msg.lines, msg.styled = styleDiff(raw, width)
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
func deleteBranchCmd(idx int, b branch, force, wantRemote bool) tea.Cmd {
	return func() tea.Msg {
		return branchDeletedMsg{idx: idx, res: deleteBranch(b, force, wantRemote)}
	}
}

// spinnerTickCmd schedules the next spinner frame.
func spinnerTickCmd() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return spinnerTickMsg{} })
}

// loadFirst runs the startup reads. The repo check runs beside them rather than
// ahead of them. It is here to give a clearer message than git's own, not to
// gate the work, and a serial subprocess start is most of what startup costs.
func loadFirst() ([]branch, repoReads, error) {
	var repoErr error
	checked := make(chan struct{})
	go func() {
		_, repoErr = runGit("rev-parse", "--is-inside-work-tree")
		close(checked)
	}()
	branches, reads, err := loadRepo(false)
	<-checked

	if repoErr != nil {
		return nil, reads, fmt.Errorf("not a git repository (or git is unavailable)")
	}
	return branches, reads, err
}

// initialModel loads the repo with every merge query answered before it
// returns. Script mode and the tests use it; the TUI uses startupModel.
func initialModel() (model, error) {
	m, err := startupModel()
	if err == nil {
		m.applyMergeInfo(m.mergeQuery()())
	}
	return m, err
}

// startupModel is initialModel without the wait for the merge queries: the list
// paints after one round of git, and Init fills the ✓ and risk columns in.
func startupModel() (model, error) {
	branches, reads, err := loadFirst()
	if err != nil {
		return model{}, err
	}
	m := model{field: sortDate, ascending: false, height: 24, width: 100}
	loadSettings(&m) // a no-op unless main set settingsPath
	m.installBranches(branches, reads)
	m.merging = true
	return m, nil
}

// sortBranches re-sorts and keeps the cursor on the branch it was on.
func (m *model) sortBranches() {
	current := ""
	if b := m.cur(); b != nil {
		current = b.name
	}
	m.sortRows()
	m.focusBranch(current)
	m.clampCursor()
}

func (m *model) sortRows() {
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
	case stateList, stateFilter, stateAge:
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

// visibleRows is how many branch rows fit: the header line and a blank above,
// a blank and the help line below, and one line each for a status and an error.
func (m model) visibleRows() int {
	used := 4
	if m.status != "" {
		used++
	}
	if m.err != "" {
		used++
	}
	return max(1, m.height-used)
}

// diffRows is how many diff lines fit between the title (2) and position (2) lines.
func (m model) diffRows() int { return max(1, m.height-4) }

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
// The local and remote views are two filters over the one slice, for the same
// reason.
func (m model) viewIdx() []int {
	idx := make([]int, 0, len(m.branches))
	f := strings.ToLower(m.filter)
	for i, b := range m.branches {
		if !m.inView(b) {
			continue
		}
		if f == "" || strings.Contains(strings.ToLower(b.name), f) {
			idx = append(idx, i)
		}
	}
	return idx
}

// inView reports whether b belongs to the view on screen, local or remote.
func (m model) inView(b branch) bool { return b.remoteOnly == m.showRemote }

func (m *model) cur() *branch {
	if idx := m.viewIdx(); m.cursor >= 0 && m.cursor < len(idx) {
		return &m.branches[idx[m.cursor]]
	}
	return nil
}

// selectedBranches returns the marked branches. Locked ones are dropped here
// too, so a mark that slipped past the keys still cannot reach a delete.
func (m model) selectedBranches() []branch {
	var out []branch
	for _, b := range m.branches {
		if b.selected && !b.locked() {
			out = append(out, b)
		}
	}
	return out
}

// Init starts the merge queries startupModel left out.
func (m model) Init() tea.Cmd {
	if !m.merging {
		return nil
	}
	gen, query := m.loadGen, m.mergeQuery()
	return func() tea.Msg { return mergeInfoMsg{gen: gen, info: query()} }
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.adjustScroll()
		if m.state == stateDiff && m.diffStyled {
			// delta laid its columns out for the old width. A window drag sends
			// dozens of sizes; wait for them to stop, so delta runs once, not
			// once per size.
			m.diffSeq++
			seq := m.diffSeq
			return m, tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return restyleTickMsg{seq} })
		}
		return m, nil
	case restyleTickMsg:
		if msg.seq != m.diffSeq || m.state != stateDiff {
			return m, nil // a later resize took over, or the user left
		}
		m.diffLoading = true
		return m, restyleCmd(m.diffSeq, m.diffRaw, m.width)
	case mergeInfoMsg:
		if msg.gen == m.loadGen {
			m.applyMergeInfo(msg.info)
		}
		return m, nil
	case diffLoadedMsg:
		if msg.seq != m.diffSeq || m.state != stateDiff {
			return m, nil // the user asked for another diff, or left
		}
		m.diffLoading = false
		if msg.err != nil {
			m.err = msg.err.Error()
			m.state = stateList
			return m, nil
		}
		if msg.base != "" {
			m.diffBase = msg.base
		}
		m.diffRaw = msg.raw
		m.diffLines, m.diffStyled = msg.lines, msg.styled
		m.clampDiff()
		return m, nil
	case undoDoneMsg:
		m.applyUndo(msg)
		return m, nil
	case repoLoadedMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.status = ""
		m.applyKeepMarks(msg.branches, msg.reads)
		return m, nil
	case fetchDoneMsg:
		m.fetching = false
		if msg.err != nil {
			m.err = msg.err.Error()
			m.status = ""
			return m, nil
		}
		if branches, reads, err := loadRepo(m.remoteLoaded); err == nil {
			// A fetch is non-destructive, so both the cursor (by name, in
			// applyBranches) and the user's pending marks survive it.
			m.applyKeepMarks(branches, reads)
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
		if msg.fromRemote {
			m.showRemote = false // the new local branch is where the user is now
		}
		if msg.loadErr == nil {
			// A switch deletes nothing, so pending marks survive it; carryMarks
			// drops the ones now on the current branch.
			m.applyKeepMarks(msg.branches, msg.reads)
		}
		m.focusBranch(msg.name)
		return m, nil
	case branchDeletedMsg:
		if msg.idx >= 0 && msg.idx < len(m.results) {
			m.results[msg.idx] = msg.res
		}
		if m.deletesDone() >= len(m.results) {
			m.reloadBranches()
			m.bodyTop, m.abortArmed = 0, false
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
		case stateAge:
			return m.updateAge(msg)
		case stateConfirm:
			return m.updateConfirm(msg)
		case stateForcePrompt:
			return m.updateForcePrompt(msg)
		case stateDiff:
			return m.updateDiff(msg)
		case stateDeleting:
			// Deletions are in flight; ignore input except an abort. Quitting
			// drops pushes not yet started, which can leave a remote copy
			// behind a deleted local branch, so the abort needs a second press.
			if msg.String() == "ctrl+c" {
				if m.abortArmed {
					return m, tea.Quit
				}
				m.abortArmed = true
			}
		case stateResult:
			switch msg.String() {
			case "q", "ctrl+c":
				return m, tea.Quit
			case "enter", "esc":
				m.state = stateList
			case "u":
				return m, m.undo()
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

// updateAge handles the merged-and-old prompt. Only digits type into it.
func (m model) updateAge(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.state = stateList
	case "enter":
		m.state = stateList
		if days, err := strconv.Atoi(m.ageInput); err == nil {
			m.err = ""
			m.status = m.selectMergedOlder(days, time.Now())
		}
	case "backspace":
		if n := len(m.ageInput); n > 0 {
			m.ageInput = m.ageInput[:n-1]
		}
	default:
		for _, r := range msg.Runes {
			if r >= '0' && r <= '9' && len(m.ageInput) < 5 {
				m.ageInput += string(r)
			}
		}
	}
	return m, nil
}

// waitForMerge reports whether a key must wait on the startup merge queries,
// and says so on the status line: selecting before they land would act on
// branches whose merge state is not yet known.
func (m *model) waitForMerge() bool {
	if m.merging {
		m.status = "still reading merge status — try again in a moment"
	}
	return m.merging
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
		if b := m.cur(); b != nil && !b.locked() {
			b.selected = !b.selected
		}
	case "r":
		if b := m.cur(); b != nil && b.remoteDeletable() {
			b.deleteRemote = !b.deleteRemote
		} else if b != nil {
			if why := b.remoteRefusal(); why != "" {
				m.status = why
			}
		}
	case "tab":
		m.showRemote = !m.showRemote
		m.cursor, m.top = 0, 0
		if m.showRemote && !m.remoteLoaded {
			m.remoteLoaded = true
			m.status = "loading remote branches…"
			return m, reloadCmd(true)
		}
	case "/":
		m.state = stateFilter
	case "m":
		m.state, m.ageInput = stateAge, strconv.Itoa(m.config.staleDays)
	case "esc":
		// n already disarms everything; esc only lifts the filter.
		m.filter = ""
		m.clampCursor()
	case "a":
		// Select what the list shows: with a filter active, a marks only the
		// matching branches, which is what makes filter-then-select useful.
		for _, i := range m.viewIdx() {
			if !m.branches[i].locked() {
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
		saveSettings(m)
	case "o":
		m.ascending = !m.ascending
		m.sortBranches()
		saveSettings(m)
	case "f":
		m.force = !m.force
	case "u":
		return m, m.undo()
	case "x":
		// The same selection `p` makes after its fetch, for a repo whose
		// gone marks are already known; no network round trip needed.
		if !m.waitForMerge() {
			m.err = ""
			m.status = m.selectGone()
		}
	case "p":
		if !m.fetching {
			m.fetching = true
			m.err = ""
			m.status = "fetching --all --prune…"
			return m, fetchPruneCmd()
		}
	case "v":
		if b := m.cur(); b != nil {
			m.err = ""
			m.diffSeq++
			m.diffBranch, m.diffBase = b.name, ""
			m.diffRaw, m.diffLines, m.diffStyled = "", nil, false
			m.diffTop, m.diffLoading = 0, true
			m.state = stateDiff
			return m, diffCmd(m.diffSeq, *b, m.width)
		}
	case "c":
		if b := m.cur(); b != nil && !b.isCurrent && !m.switching && !m.fetching {
			m.switching = true
			m.err = ""
			m.status = "switching to " + b.name + "…"
			return m, switchCmd(*b, m.remoteLoaded)
		}
	case "?":
		m.state = stateHelp
	case "d", "enter":
		if len(m.selectedBranches()) > 0 && !m.waitForMerge() {
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
		if m.localSelectedCount() > 0 {
			return m, m.startDeletions(false)
		}
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
		if b.remoteArmed() {
			n++
		}
	}
	return n
}

// armedRemoteCount counts selected branches whose remote deletion is armed.
func (m model) armedRemoteCount() int {
	return countArmedRemotes(m.selectedBranches())
}

// localSelectedCount counts selected branches that have a local branch to delete.
func (m model) localSelectedCount() int {
	n := 0
	for _, b := range m.selectedBranches() {
		if !b.remoteOnly {
			n++
		}
	}
	return n
}

// startDeletions kicks off the asynchronous deletion of the selected branches,
// pre-seeding results, entering stateDeleting, and returning a batch of one cmd
// per branch (which run concurrently) plus the spinner tick. Remote branches are
// pushed --delete only when includeRemote is set (see updateConfirm), so a
// remoteOnly row is left out of a local-only run.
func (m *model) startDeletions(includeRemote bool) tea.Cmd {
	m.measureSelectedRisk() // results carry the cost through to the force prompt
	var sel []branch
	for _, b := range m.selectedBranches() {
		if includeRemote || !b.remoteOnly {
			sel = append(sel, b)
		}
	}
	m.history = append(m.history, m.results...)
	m.results = make([]deleteResult, len(sel))
	m.spinnerFrame, m.abortArmed = 0, false
	m.state, m.bodyTop = stateDeleting, 0

	cmds := []tea.Cmd{spinnerTickCmd()}
	for i, b := range sel {
		m.results[i] = deleteResult{br: b}
		wantRemote := includeRemote && b.remoteArmed()
		cmds = append(cmds, deleteBranchCmd(i, b, m.force, wantRemote))
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
	m.diffTop = max(0, min(m.diffTop, len(m.diffLines)-m.diffRows()))
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
		m.diffTop -= m.diffRows() / 2
		m.clampDiff()
	case "ctrl+d", "pgdown", " ":
		m.diffTop += m.diffRows() / 2
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
// branch set, matching by ref. Used on the fetch path, which reloads every
// branch struct but changes nothing the marks were made about. An armed remote
// delete is dropped when the fetch reveals the upstream is already gone: the push
// it would run can only fail.
func carryMarks(old, fresh []branch) []branch {
	prev := make(map[string]branch, len(old))
	for _, b := range old {
		prev[b.ref()] = b
	}
	for i := range fresh {
		if p, ok := prev[fresh[i].ref()]; ok {
			// Marks never land on the current branch: it cannot be deleted, so
			// a carried mark would arm an operation the list refuses to offer
			// (relevant after a switch, or when HEAD moved outside the TUI).
			// Protection is applied after this, and clears its own marks.
			fresh[i].selected = p.selected && !fresh[i].locked()
			fresh[i].deleteRemote = p.deleteRemote && !fresh[i].gone && !fresh[i].locked()
		}
	}
	return fresh
}

// applyBranches installs a freshly-loaded branch set and recomputes everything
// derived from it (name-width, merge info, sort order). The cursor stays on the
// branch it was on. This is the single refresh core shared by every reload.
func (m *model) applyBranches(branches []branch, reads repoReads) {
	m.installBranches(branches, reads)
	m.applyMergeInfo(m.mergeQuery()())
}

// applyKeepMarks is applyBranches for a reload that deleted nothing, so the
// user's pending marks carry over by ref.
func (m *model) applyKeepMarks(branches []branch, reads repoReads) {
	m.applyBranches(carryMarks(m.branches, branches), reads)
}

// installBranches is applyBranches without the merge queries, which cost a
// round of git. startupModel runs them in the background instead.
func (m *model) installBranches(branches []branch, reads repoReads) {
	// Read the cursor's branch from the old list, before it is replaced: after,
	// the cursor index points into an unsorted new list.
	focus := ""
	if b := m.cur(); b != nil {
		focus = b.name
	}
	m.loadGen++
	m.merging = false
	m.config = reads.config
	m.branches = append(branches, remoteOnlyRows(branches, reads.remotes.rows)...)
	m.recomputeNameWidth()
	m.resolveBases(reads)
	for i := range m.branches {
		b := &m.branches[i]
		b.headMerged = !b.remoteOnly && reads.headMerged[b.name]
		b.protected = m.isProtected(*b)
		// The trunk and protect checks apply to the remote branch r would delete,
		// not only to the local name: a branch made from origin/main tracks main.
		b.remoteProtected = !b.remoteOnly && b.upstream != "" && m.protectsName(b.remoteBranch())
		if b.locked() {
			b.selected = false
		}
		if !b.remoteDeletable() {
			b.deleteRemote = false
		}
	}
	m.sortRows()
	m.cursor = 0
	m.focusBranch(focus)
	m.clampCursor()
}

// remoteOnlyRows keeps the remote rows that no local branch tracks.
func remoteOnlyRows(local, remote []branch) []branch {
	tracked := make(map[string]bool, len(local))
	for _, b := range local {
		tracked[b.upstream] = true
	}
	var out []branch
	for _, r := range remote {
		if !tracked[r.name] {
			out = append(out, r)
		}
	}
	return out
}

// isProtected reports whether b is the trunk or matches a pruner.protect glob.
// A remote row is matched by its branch part, so "release/*" covers both views.
func (m model) isProtected(b branch) bool {
	if b.remoteOnly {
		return b.name == m.remoteDefault || m.protectsName(b.remoteBranch())
	}
	return m.protectsName(b.name)
}

// protectsName reports whether a branch of this name, local or on a remote, is
// the trunk or matches a pruner.protect glob.
func (m model) protectsName(name string) bool {
	if _, trunk, ok := strings.Cut(m.remoteDefault, "/"); (ok && name == trunk) || branchRef(name) == m.riskBaseRef {
		return true
	}
	for _, p := range m.config.protect {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// reloadBranches refreshes the branch list from git and resets the view to the
// top — appropriate after a mutation that may have removed the cursor's branch.
func (m *model) reloadBranches() {
	if branches, reads, err := loadRepo(m.remoteLoaded); err == nil {
		m.applyBranches(branches, reads)
		m.cursor, m.top = 0, 0
	}
}

// hasBranch reports whether the loaded list holds a local branch of that name.
func (m model) hasBranch(name string) bool {
	for _, b := range m.branches {
		if b.name == name && !b.remoteOnly {
			return true
		}
	}
	return false
}

// resolveBases finds the remote default and the risk base. It needs no git
// call: the refs came with the load, and the local fallback is a lookup.
// The refs drive git; the short forms are what the views print.
func (m *model) resolveBases(reads repoReads) {
	m.remoteDefaultRef = remoteDefaultFrom(reads.remotes)
	m.riskBaseRef = m.remoteDefaultRef
	if m.riskBaseRef == "" {
		m.riskBaseRef = localDefaultBranch("", m.hasBranch)
	}
	m.remoteDefault = shortRef(m.remoteDefaultRef)
	m.riskBase = shortRef(m.riskBaseRef)
}

// goneRefs lists the refs of the gone branches. Only these are measured up
// front, because `p` consults the count to decide what it may auto-select. The
// rest wait for measureSelectedRisk: riskCommitCount is a subprocess per branch,
// and measuring every unmergeable branch here would put a network-free repo's
// whole branch list on the clock.
func (m model) goneRefs() []string {
	var out []string
	for _, b := range m.branches {
		if b.gone {
			out = append(out, b.ref())
		}
	}
	return out
}

// mergeQuery captures the merge queries' inputs now and returns the call. The
// inputs are read here, on the update loop, so a goroutine running the call
// never reads the branch list while Update changes it.
func (m model) mergeQuery() func() mergeInfo {
	defRef, baseRef, gone := m.remoteDefaultRef, m.riskBaseRef, m.goneRefs()
	return func() mergeInfo { return queryMergeInfo(defRef, baseRef, gone) }
}

// queryMergeInfo runs the merge queries. It reads no model state, so it can run
// in a tea.Cmd. Neither query needs the other's answer, and each is a
// subprocess start, so they go together.
func queryMergeInfo(defRef, baseRef string, gone []string) mergeInfo {
	var info mergeInfo
	remote := make(chan struct{})
	go func() {
		info.remoteMerged = remoteMergedSet(defRef)
		close(remote)
	}()
	// One query answers "is this branch's tip already in the base?" for the whole
	// list, which is the answer for most branches a prune touches.
	if baseRef != "" {
		info.baseMerged = mergedSet("branch", "--merged", baseRef)
	}
	<-remote

	var todo []string
	info.risk = map[string]int{}
	for _, ref := range gone {
		if info.baseMerged[shortRef(ref)] {
			info.risk[ref] = 0
		} else {
			todo = append(todo, ref)
		}
	}
	for i, n := range countRisk(todo, baseRef) {
		info.risk[todo[i]] = n
	}
	return info
}

// applyMergeInfo writes the merge query results onto the branches.
func (m *model) applyMergeInfo(info mergeInfo) {
	m.merging = false
	m.baseMerged = info.baseMerged
	for i := range m.branches {
		b := &m.branches[i]
		b.remoteMerged = b.upstream != "" && info.remoteMerged[b.upstream]
		b.riskMeasured = false // the branch was just reloaded; any old count is stale
		if n, ok := info.risk[b.ref()]; ok {
			b.riskCommits, b.riskMeasured = n, true
		}
	}
}

// inBase reports whether b's tip is already in the risk base, from the merge
// queries alone. Such a branch has an empty base..branch range, so `git cherry`
// would report nothing.
func (m model) inBase(b branch) bool {
	// A remote row is only measured when it is not merged (see
	// measureSelectedRisk), so the local set is the only one worth asking.
	return !b.remoteOnly && m.baseMerged[b.name]
}

// countRisk runs riskCommitCountRef for each ref. The counts are independent
// `git cherry` subprocesses whose cost is dominated by process spawn, so they
// run concurrently: measured one at a time, a repo with a hundred gone branches
// spent 1.1s here on every load, fetch and prune.
func countRisk(refs []string, base string) []int {
	out := make([]int, len(refs))
	var wg sync.WaitGroup
	for i, ref := range refs {
		// Each goroutine owns one slice element, so no two write the same one.
		wg.Go(func() { out[i] = riskCommitCountRef(ref, base) })
	}
	wg.Wait()
	return out
}

// measureRisk fills in the cost-of-deletion count for every not-yet-measured
// branch that want accepts.
func (m *model) measureRisk(want func(branch) bool) {
	var idx []int
	var refs []string
	for i, b := range m.branches {
		if b.riskMeasured || !want(b) {
			continue
		}
		// Answer from the merge sets instead of spawning the subprocess.
		if m.inBase(b) {
			m.branches[i].riskCommits, m.branches[i].riskMeasured = 0, true
			continue
		}
		idx = append(idx, i)
		refs = append(refs, b.ref())
	}
	for k, n := range countRisk(refs, m.riskBaseRef) {
		m.branches[idx[k]].riskCommits, m.branches[idx[k]].riskMeasured = n, true
	}
}

// measureSelectedRisk measures what deleting each selected branch would cost.
// Call before any view that reports the cost: only branches a safe delete would
// refuse are measured, since those are the ones deleted with -D. A remote
// branch not merged into the default is measured too: deleting it can take the
// only shared copy of its commits.
func (m *model) measureSelectedRisk() {
	m.measureRisk(func(b branch) bool {
		return b.selected && (b.gone || !b.safeDeletable() || (b.remoteOnly && !b.remoteMerged))
	})
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
		// deleteBranch kept the worktree while -d was going to refuse the
		// branch; the user has now agreed to the delete, so free it.
		if r.worktreePending() && !removeWorktree(r) {
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

// undoDoneMsg carries a background undo and the reload after it.
type undoDoneMsg struct {
	restored []int // indices into m.results
	errs     []string
	branches []branch
	reads    repoReads
	loadErr  error
}

// undo recreates the local branches the last run deleted, at the commits they
// pointed to, with their upstream config. It runs as a tea.Cmd: each branch is
// up to three git calls, and a wide prune would freeze the screen for seconds.
// The calls stay serial because parallel `git config` writes collide on
// .git/config.lock. A remote branch cannot come back this way: that would be a
// push, so the exit summary prints the command instead.
func (m *model) undo() tea.Cmd {
	var todo []int
	for i, r := range m.results {
		if r.restorable() {
			todo = append(todo, i)
		}
	}
	m.state = stateList
	if len(todo) == 0 {
		m.status = "nothing to undo"
		return nil
	}
	m.status = "restoring…"
	results, withRemote := slices.Clone(m.results), m.remoteLoaded
	return func() tea.Msg {
		var msg undoDoneMsg
		for _, i := range todo {
			if err := restoreBranch(results[i].br); err != nil {
				msg.errs = append(msg.errs, results[i].br.name+": "+err.Error())
			} else {
				msg.restored = append(msg.restored, i)
			}
		}
		msg.branches, msg.reads, msg.loadErr = loadRepo(withRemote)
		return msg
	}
}

// applyUndo records a finished undo and installs the reload that came with it.
func (m *model) applyUndo(msg undoDoneMsg) {
	remote := 0
	for _, i := range msg.restored {
		m.results[i].restored = true
	}
	for _, r := range m.results {
		if r.remoteOK {
			remote++
		}
	}
	if msg.loadErr == nil {
		m.applyKeepMarks(msg.branches, msg.reads)
	}
	m.err = strings.Join(msg.errs, "; ")
	m.status = fmt.Sprintf("restored %d branch(es)", len(msg.restored))
	if remote > 0 {
		m.status += fmt.Sprintf(" · %d remote branch(es) not restored — see the commands printed on quit", remote)
	}
}

// selectGone selects every gone branch that carries nothing missing from the
// base and reports the outcome for the status line. Branches holding unique
// commits stay unselected so discarding them is a deliberate keystroke rather
// than a side effect of `p` or `x`.
func (m *model) selectGone() string {
	gone, risky, unknown := 0, 0, 0
	for i := range m.branches {
		br := &m.branches[i]
		if !br.gone || br.locked() {
			continue
		}
		gone++
		switch {
		case br.riskCommits > 0:
			risky++
			continue
		case br.riskCommits == riskUnknown:
			unknown++
			continue
		}
		br.selected = true
	}
	switch {
	case gone == 0:
		return "no gone branches"
	case risky+unknown == 0:
		return fmt.Sprintf("%d gone branch(es) selected; press d to prune", gone)
	}
	msg := fmt.Sprintf("%d of %d gone branch(es) selected", gone-risky-unknown, gone)
	if risky > 0 {
		msg += fmt.Sprintf("; %d hold commits not in %s", risky, m.riskBase)
	}
	if unknown > 0 {
		msg += fmt.Sprintf("; %d could not be checked", unknown)
	}
	return msg + " (select with space to discard)"
}

// merged reports whether b's work is already in the default branch. A local
// branch ahead of its upstream holds commits the upstream check cannot see, so
// it only counts when its own tip is in the base.
func (m model) merged(b branch) bool {
	if b.remoteOnly {
		return b.remoteMerged
	}
	return m.baseMerged[b.name] || (b.remoteMerged && b.ahead == 0)
}

// selectMergedOlder selects the listed branches that are merged and whose last
// commit is older than days. It acts on the listed rows only, like `a`.
func (m *model) selectMergedOlder(days int, now time.Time) string {
	cutoff := now.AddDate(0, 0, -days)
	n := 0
	for _, i := range m.viewIdx() {
		b := &m.branches[i]
		if b.locked() || !m.merged(*b) || !b.committed.Before(cutoff) {
			continue
		}
		if !b.selected {
			b.selected = true
			n++
		}
	}
	return fmt.Sprintf("selected %d merged branch(es) older than %d days", n, days)
}
