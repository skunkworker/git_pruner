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
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
)

type deleteResult struct {
	name        string
	ahead       int  // commits the branch was ahead of upstream (for the force prompt)
	done        bool // the async deletion for this branch has completed
	localOK     bool
	localErr    string
	forceable   bool // a safe (-d) delete failed and could be retried with -D
	remoteTried bool
	remoteOK    bool
	remoteErr   string
}

type model struct {
	branches []branch
	cursor   int
	top      int // index of first visible row (scroll window)

	field     sortField
	ascending bool
	force     bool
	nameW     int // cached branch-name column width (see recomputeNameWidth)

	state   viewState
	results []deleteResult

	remoteDefault string // resolved remote default branch, e.g. "origin/main"

	spinnerFrame int // animation frame for the deleting spinner (deletion counts derive from results)

	diffBranch string   // branch whose diff is shown in stateDiff
	diffBase   string   // base ref the diff was computed against
	diffLines  []string // raw lines of the diff being viewed
	diffTop    int      // scroll offset within diffLines

	width, height int
	err           string
	status        string // transient info message (e.g. fetch results)
	fetching      bool   // a background fetch --all --prune is in flight
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

func runGit(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
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

var trackRe = regexp.MustCompile(`ahead (\d+)|behind (\d+)`)

func loadBranches() ([]branch, error) {
	const format = "%(refname:short)%00%(objectname:short)%00%(committerdate:iso8601-strict)%00" +
		"%(committerdate:relative)%00%(upstream:short)%00%(upstream:track)%00%(HEAD)%00%(contents:subject)"
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
			name:         f[0],
			hash:         f[1],
			committedRel: f[3],
			upstream:     f[4],
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

// baseBranch returns a reference to diff a branch against: the repo's default
// branch (origin/HEAD, else main, else master), excluding name itself.
func baseBranch(name string) string {
	if out, err := runGit("symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if s := strings.TrimSpace(out); s != "" && s != name {
			return s
		}
	}
	for _, c := range []string{"main", "master"} {
		if c == name {
			continue
		}
		if _, err := runGit("rev-parse", "--verify", "--quiet", c); err == nil {
			return c
		}
	}
	return ""
}

// remoteDefault resolves the remote's default branch as a remote-tracking ref
// (e.g. "origin/main"): origin/HEAD if set, else origin/main, else origin/master.
// Returns "" when none can be determined.
func remoteDefault() string {
	if out, err := runGit("symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if s := strings.TrimSpace(out); s != "" {
			return s
		}
	}
	for _, c := range []string{"origin/main", "origin/master"} {
		if _, err := runGit("rev-parse", "--verify", "--quiet", "refs/remotes/"+c); err == nil {
			return c
		}
	}
	return ""
}

// remoteMergedSet returns the set of remote-tracking branches (short names, e.g.
// "origin/feature") whose tip is merged into def. Operates on local
// remote-tracking refs, so it needs no network — it reflects the last fetch.
func remoteMergedSet(def string) map[string]bool {
	set := map[string]bool{}
	if def == "" {
		return set
	}
	out, err := runGit("branch", "-r", "--merged", def, "--format=%(refname:short)")
	if err != nil {
		return set
	}
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			set[s] = true
		}
	}
	return set
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
		res := deleteBranch(b.name, flag, wantRemote, b.remoteName(), b.remoteBranch(), b.ahead)
		return branchDeletedMsg{idx: idx, res: res}
	}
}

// spinnerTickCmd schedules the next spinner frame.
func spinnerTickCmd() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return spinnerTickMsg{} })
}

// loadDiff returns the patch introduced on name relative to its merge-base with
// the repo's default branch — i.e. what the branch contains — and the base ref used.
func loadDiff(name string) (diff, base string, err error) {
	base = baseBranch(name)
	if base == "" {
		base = "HEAD"
	}
	diff, err = runGit("diff", base+"..."+name)
	return diff, base, err
}

// ---- model ----

func initialModel() (model, error) {
	if _, err := runGit("rev-parse", "--is-inside-work-tree"); err != nil {
		return model{}, fmt.Errorf("not a git repository (or git is unavailable)")
	}
	branches, err := loadBranches()
	if err != nil {
		return model{}, err
	}
	m := model{branches: branches, field: sortDate, ascending: false, height: 24, width: 100}
	m.recomputeNameWidth()
	m.refreshMergeInfo()
	m.sortBranches()
	return m, nil
}

func (m *model) sortBranches() {
	current := ""
	if m.cursor < len(m.branches) {
		current = m.branches[m.cursor].name
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
	for i, b := range m.branches {
		if b.name == current {
			m.cursor = i
			break
		}
	}
	m.clampCursor()
}

func (m *model) clampCursor() {
	m.cursor = max(0, min(m.cursor, len(m.branches)-1))
	m.adjustScroll()
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

func (m *model) cur() *branch {
	if m.cursor >= 0 && m.cursor < len(m.branches) {
		return &m.branches[m.cursor]
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
		if branches, err := loadBranches(); err == nil {
			m.applyBranches(branches) // preserves the cursor by name (fetch is non-destructive)
		}
		gone := 0
		for i := range m.branches {
			if m.branches[i].gone && !m.branches[i].isCurrent {
				m.branches[i].selected = true
				gone++
			}
		}
		m.err = ""
		if gone > 0 {
			m.status = fmt.Sprintf("fetched & pruned — %d gone branch(es) selected; press d to prune", gone)
		} else {
			m.status = "fetched & pruned — no gone branches"
		}
		return m, nil
	case branchDeletedMsg:
		if msg.idx >= 0 && msg.idx < len(m.results) {
			m.results[msg.idx] = msg.res
		}
		if m.deletesDone() >= len(m.results) {
			m.reloadBranches()
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
	case tea.KeyMsg:
		switch m.state {
		case stateList:
			return m.updateList(msg)
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
		m.cursor = len(m.branches) - 1
		m.clampCursor()
	case " ":
		if b := m.cur(); b != nil && !b.isCurrent {
			b.selected = !b.selected
		}
	case "r":
		if b := m.cur(); b != nil && b.upstream != "" && !b.isCurrent {
			b.deleteRemote = !b.deleteRemote
		}
	case "a":
		for i := range m.branches {
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
	case "?":
		m.state = stateHelp
	case "d", "enter":
		if len(m.selectedBranches()) > 0 {
			m.state = stateConfirm
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
	sel := m.selectedBranches()
	m.results = make([]deleteResult, len(sel))
	m.spinnerFrame = 0
	m.state = stateDeleting

	cmds := []tea.Cmd{spinnerTickCmd()}
	for i, b := range sel {
		m.results[i] = deleteResult{name: b.name, ahead: b.ahead}
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
		m.state = stateResult
	case "n", "N", "esc", "q", "enter":
		m.state = stateResult
	case "ctrl+c":
		return m, tea.Quit
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

// deleteFlag returns the git branch delete flag for b under the given force mode:
// -D for forced or gone branches (which -d refuses), -d otherwise.
func (b branch) deleteFlag(force bool) string {
	if force || b.gone {
		return "-D"
	}
	return "-d"
}

// deleteBranch runs one branch's local delete and, when wantRemote is set, its
// remote-branch push --delete. It is the single worker shared by the synchronous
// performDeletions path and the asynchronous deleteBranchCmd path.
func deleteBranch(name, flag string, wantRemote bool, remoteName, remoteBranch string, ahead int) deleteResult {
	res := deleteResult{name: name, ahead: ahead, done: true}
	if _, err := runGit("branch", flag, name); err != nil {
		res.localErr = err.Error()
		res.forceable = flag == "-d" // a refused safe delete can be retried with -D
	} else {
		res.localOK = true
	}
	if wantRemote {
		res.remoteTried = true
		if _, err := runGit("push", remoteName, "--delete", remoteBranch); err != nil {
			res.remoteErr = err.Error()
		} else {
			res.remoteOK = true
		}
	}
	return res
}

// performDeletions deletes the selected branches synchronously. The interactive
// UI uses the async startDeletions path instead; this remains for tests and as
// the straightforward equivalent.
func (m *model) performDeletions() {
	m.results = nil
	for _, b := range m.selectedBranches() {
		wantRemote := b.deleteRemote && b.upstream != ""
		res := deleteBranch(b.name, b.deleteFlag(m.force), wantRemote, b.remoteName(), b.remoteBranch(), b.ahead)
		m.results = append(m.results, res)
	}
	m.reloadBranches()
}

// applyBranches installs a freshly-loaded branch set and recomputes everything
// derived from it (name-width, merge info, sort order). Callers set their own
// cursor policy around it. This is the single refresh core shared by
// reloadBranches and the fetch handler.
func (m *model) applyBranches(branches []branch) {
	m.branches = branches
	m.recomputeNameWidth()
	m.refreshMergeInfo()
	m.sortBranches()
}

// reloadBranches refreshes the branch list from git and resets the view to the
// top — appropriate after a mutation that may have removed the cursor's branch.
func (m *model) reloadBranches() {
	if branches, err := loadBranches(); err == nil {
		m.cursor = 0
		m.top = 0
		m.applyBranches(branches)
	}
}

// refreshMergeInfo caches the remote default branch and marks each branch whose
// upstream is merged into it. Call after every branch (re)load.
func (m *model) refreshMergeInfo() {
	m.remoteDefault = remoteDefault()
	merged := remoteMergedSet(m.remoteDefault)
	for i := range m.branches {
		m.branches[i].remoteMerged = m.branches[i].upstream != "" && merged[m.branches[i].upstream]
	}
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
		if _, err := runGit("branch", "-D", r.name); err != nil {
			r.localErr = err.Error()
		} else {
			r.localOK = true
			r.localErr = ""
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
		w = max(w, len(br.name))
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
	header := fmt.Sprintf("git_pruner — %d branches   sort: %s %s   delete mode: %s",
		len(m.branches), m.field, dir, forceLabel)
	b.WriteString(headerStyle.Render(header))
	b.WriteString("\n\n")

	if len(m.branches) == 0 {
		b.WriteString(dimStyle.Render("no local branches found"))
		b.WriteString("\n")
	}

	nameW := m.nameW
	vis := m.visibleRows()
	end := min(m.top+vis, len(m.branches))
	for i := m.top; i < end; i++ {
		b.WriteString(m.renderRow(i, nameW))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	help := "↑/↓ move · space select · a/n all/none · r remote · v view · p prune · s sort · o order · f force · d delete · ? help · q quit"
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

func (m model) renderRow(i, nameW int) string {
	br := m.branches[i]

	cursor := "  "
	if i == m.cursor {
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

	name := fmt.Sprintf("%-*s", nameW, truncate(br.name, nameW))

	var nameRendered string
	switch {
	case i == m.cursor:
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
		return goneStyle.Render(fmt.Sprintf("%-8s", "gone"))
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

func truncate(s string, w int) string {
	if len(s) <= w {
		return s
	}
	if w <= 1 {
		return ""
	}
	return s[:w-1] + "…"
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
		{"a / n", "select all / none"},
		{"r", "toggle delete of upstream remote branch"},
		{"v", "view branch diff (green add / red remove)"},
		{"p", "fetch --all --prune & select gone branches"},
		{"s", "cycle sort field (date, name, ahead/behind)"},
		{"o", "toggle sort order (asc/desc)"},
		{"f", "toggle force delete (-d / -D)"},
		{"d, enter", "delete selected branches (local)"},
		{"", "on confirm: y = local only · R = local + remote"},
		{"", "(unmerged -d failures prompt to retry with -D)"},
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
	b.WriteString(dimStyle.Render("press any key to return"))
	b.WriteString("\n")
	return b.String()
}

func (m model) confirmView() string {
	var b strings.Builder
	sel := m.selectedBranches()

	b.WriteString(headerStyle.Render("Confirm deletion"))
	b.WriteString("\n\n")
	flag := "-d (safe)"
	if m.force {
		flag = "-D (force)"
	}
	remoteCount := countArmedRemotes(sel)
	b.WriteString(fmt.Sprintf("Local delete mode: %s\n", flag))
	b.WriteString(fmt.Sprintf("Deleting %d local branch(es), %d remote branch(es).\n\n", len(sel), remoteCount))

	for _, br := range sel {
		b.WriteString("  " + cursorStyle.Render("• "+br.name) + "\n")

		date := br.committed.Format("2006-Jan-02")
		if br.committedRel != "" {
			date += " (" + br.committedRel + ")"
		}
		b.WriteString("      " + dimStyle.Render(fmt.Sprintf("%s  %s  %s", br.hash, date, truncate(br.subject, 50))) + "\n")

		switch {
		case br.gone:
			b.WriteString("      " + goneStyle.Render("upstream gone: "+br.upstream+" — will prune with -D (force)") + "\n")
		case br.upstream != "":
			b.WriteString("      " + dimStyle.Render("upstream: "+br.upstream) + " " + m.trackStr(br) + "\n")
		default:
			b.WriteString("      " + dimStyle.Render("no upstream") + "\n")
		}

		if br.upstream != "" && m.remoteDefault != "" {
			if br.remoteMerged {
				b.WriteString("      " + okStyle.Render("✓ merged into "+m.remoteDefault) + "\n")
			} else {
				b.WriteString("      " + goneStyle.Render("⚠ not merged into "+m.remoteDefault) + "\n")
			}
		}

		if br.deleteRemote && br.upstream != "" {
			b.WriteString("      " + errStyle.Render(fmt.Sprintf("+ delete remote %s/%s", br.remoteName(), br.remoteBranch())) + "\n")
		}
		if !m.force && !br.gone && br.ahead > 0 {
			b.WriteString("      " + errStyle.Render(fmt.Sprintf("⚠ %d unmerged commit(s) — safe delete (-d) will fail; use force (f)", br.ahead)) + "\n")
		}
		b.WriteString("\n")
	}

	b.WriteString(headerStyle.Render("Delete these branches? "))
	if remoteCount > 0 {
		b.WriteString(dimStyle.Render(fmt.Sprintf("(y = local only · R = local + remote (%d) · n/esc = cancel)", remoteCount)))
	} else {
		b.WriteString(dimStyle.Render("(y = yes · n/esc = cancel)"))
	}
	b.WriteString("\n")
	return b.String()
}

func (m model) forcePromptView() string {
	var b strings.Builder
	failures := m.forceableFailures()

	b.WriteString(headerStyle.Render("Force delete unmerged branches?"))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("%d branch(es) were refused by safe delete (-d) because they are not\n", len(failures)))
	b.WriteString("fully merged. Force deleting (-D) will ")
	b.WriteString(errStyle.Render("permanently discard their unmerged commits"))
	b.WriteString(".\n\n")

	for _, r := range failures {
		b.WriteString("  " + cursorStyle.Render("• "+r.name) + "\n")
		if r.ahead > 0 {
			b.WriteString("      " + errStyle.Render(fmt.Sprintf("⚠ %d unmerged commit(s) will be lost", r.ahead)) + "\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(headerStyle.Render("Force delete (-D) these branches? "))
	b.WriteString(dimStyle.Render("(y = yes, discard · n/esc = keep them)"))
	b.WriteString("\n")
	return b.String()
}

// writeResultLines renders one completed deletion result (local, then remote if
// tried) into b. Shared by the results screen and the live deleting screen.
func writeResultLines(b *strings.Builder, r deleteResult) {
	if r.localOK {
		b.WriteString(okStyle.Render("  ✓ ") + "deleted local " + r.name + "\n")
	} else {
		b.WriteString(errStyle.Render("  ✗ ") + "local " + r.name + ": " + r.localErr + "\n")
	}
	if r.remoteTried {
		if r.remoteOK {
			b.WriteString(okStyle.Render("  ✓ ") + "deleted remote " + r.name + "\n")
		} else {
			b.WriteString(errStyle.Render("  ✗ ") + "remote " + r.name + ": " + r.remoteErr + "\n")
		}
	}
}

func (m model) deletingView() string {
	var b strings.Builder
	spin := spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
	b.WriteString(headerStyle.Render(fmt.Sprintf("%s Deleting… (%d/%d)", spin, m.deletesDone(), len(m.results))))
	b.WriteString("\n\n")
	for _, r := range m.results {
		if r.done {
			writeResultLines(&b, r)
		} else {
			b.WriteString(dimStyle.Render("  "+spin+" deleting "+r.name+"…") + "\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("working — ctrl+c to abort"))
	b.WriteString("\n")
	return b.String()
}

func (m model) resultView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Results"))
	b.WriteString("\n\n")
	for _, r := range m.results {
		writeResultLines(&b, r)
	}
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("press q/enter to quit"))
	b.WriteString("\n")
	return b.String()
}

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
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "git_pruner:", err)
		os.Exit(1)
	}
}
