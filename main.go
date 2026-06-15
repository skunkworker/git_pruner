package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
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
	isCurrent    bool
	selected     bool
	deleteRemote bool
}

func (b branch) remoteName() string {
	if i := strings.IndexByte(b.upstream, '/'); i >= 0 {
		return b.upstream[:i]
	}
	return "origin"
}

func (b branch) remoteBranch() string {
	if i := strings.IndexByte(b.upstream, '/'); i >= 0 {
		return b.upstream[i+1:]
	}
	return b.name
}

type sortField int

const (
	sortDate sortField = iota
	sortName
	sortAheadBehind
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
	stateResult
)

type deleteResult struct {
	name        string
	localOK     bool
	localErr    string
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

	state   viewState
	results []deleteResult

	width, height int
	err           string
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
	if m.cursor >= len(m.branches) {
		m.cursor = len(m.branches) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.adjustScroll()
}

func (m *model) visibleRows() int {
	r := m.height - 5 // header (2) + footer (3)
	if r < 1 {
		r = 1
	}
	return r
}

func (m *model) adjustScroll() {
	vis := m.visibleRows()
	if m.cursor < m.top {
		m.top = m.cursor
	}
	if m.cursor >= m.top+vis {
		m.top = m.cursor - vis + 1
	}
	if m.top < 0 {
		m.top = 0
	}
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
	case tea.KeyMsg:
		switch m.state {
		case stateList:
			return m.updateList(msg)
		case stateConfirm:
			return m.updateConfirm(msg)
		case stateResult:
			switch msg.String() {
			case "q", "ctrl+c", "enter", "esc":
				return m, tea.Quit
			}
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
		m.field = (m.field + 1) % 3
		m.sortBranches()
	case "o":
		m.ascending = !m.ascending
		m.sortBranches()
	case "f":
		m.force = !m.force
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
		m.performDeletions()
		m.state = stateResult
	case "n", "N", "esc", "q":
		m.state = stateList
	case "ctrl+c":
		return m, tea.Quit
	}
	return m, nil
}

func (m *model) performDeletions() {
	m.results = nil
	for _, b := range m.selectedBranches() {
		res := deleteResult{name: b.name}
		flag := "-d"
		if m.force {
			flag = "-D"
		}
		if _, err := runGit("branch", flag, b.name); err != nil {
			res.localErr = err.Error()
		} else {
			res.localOK = true
		}
		if b.deleteRemote && b.upstream != "" {
			res.remoteTried = true
			if _, err := runGit("push", b.remoteName(), "--delete", b.remoteBranch()); err != nil {
				res.remoteErr = err.Error()
			} else {
				res.remoteOK = true
			}
		}
		m.results = append(m.results, res)
	}
	if branches, err := loadBranches(); err == nil {
		m.branches = branches
		m.cursor = 0
		m.top = 0
		m.sortBranches()
	}
}

// ---- views ----

func (m model) View() string {
	switch m.state {
	case stateConfirm:
		return m.confirmView()
	case stateResult:
		return m.resultView()
	default:
		return m.listView()
	}
}

func (m model) nameWidth() int {
	w := 0
	for _, br := range m.branches {
		if len(br.name) > w {
			w = len(br.name)
		}
	}
	if w > 40 {
		w = 40
	}
	if w < 6 {
		w = 6
	}
	return w
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

	nameW := m.nameWidth()
	vis := m.visibleRows()
	end := m.top + vis
	if end > len(m.branches) {
		end = len(m.branches)
	}
	for i := m.top; i < end; i++ {
		b.WriteString(m.renderRow(i, nameW))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	help := "↑/↓ move · space select · a/n all/none · r remote · s sort · o order · f force · d delete · q quit"
	b.WriteString(dimStyle.Render(help))
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
		cursor = "> "
	}
	sel := "[ ]"
	if br.selected {
		sel = "[x]"
	}
	rem := " "
	if br.deleteRemote {
		rem = "R"
	}
	cur := " "
	if br.isCurrent {
		cur = "*"
	}

	name := br.name
	if len(name) > nameW {
		name = name[:nameW-1] + "…"
	}
	name = fmt.Sprintf("%-*s", nameW, name)

	track := m.trackStr(br)
	rel := fmt.Sprintf("%-13s", br.committedRel)
	hash := fmt.Sprintf("%-8s", br.hash)

	line := fmt.Sprintf("%s%s %s %s %s  %s %s %s %s",
		cursor, sel, rem, cur, name, track,
		dimStyle.Render(rel), dimStyle.Render(hash), truncate(br.subject, m.subjectWidth(nameW)))

	switch {
	case i == m.cursor:
		return cursorStyle.Render(line)
	case br.isCurrent:
		return currentStyle.Render(line)
	case br.selected:
		return selStyle.Render(line)
	default:
		return line
	}
}

func (m model) trackStr(br branch) string {
	if br.gone {
		return goneStyle.Render(fmt.Sprintf("%-8s", "gone"))
	}
	if br.upstream == "" {
		return fmt.Sprintf("%-8s", "-")
	}
	s := ""
	if br.ahead > 0 {
		s += "↑" + strconv.Itoa(br.ahead)
	}
	if br.behind > 0 {
		s += "↓" + strconv.Itoa(br.behind)
	}
	if s == "" {
		s = "="
	}
	return fmt.Sprintf("%-8s", s)
}

func (m model) subjectWidth(nameW int) int {
	used := 2 + 3 + 1 + 1 + 1 + 1 + 1 + 1 + nameW + 2 + 8 + 1 + 13 + 1 + 8 + 1
	w := m.width - used
	if w < 10 {
		w = 10
	}
	return w
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

func (m model) confirmView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Confirm deletion"))
	b.WriteString("\n\n")
	flag := "-d (safe)"
	if m.force {
		flag = "-D (force)"
	}
	b.WriteString(fmt.Sprintf("Local delete mode: %s\n\n", flag))
	for _, br := range m.selectedBranches() {
		line := "  • " + br.name
		if br.deleteRemote && br.upstream != "" {
			line += errStyle.Render(fmt.Sprintf("   + remote %s/%s", br.remoteName(), br.remoteBranch()))
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(headerStyle.Render("Delete these branches? "))
	b.WriteString(dimStyle.Render("(y = yes, n/esc = cancel)"))
	b.WriteString("\n")
	return b.String()
}

func (m model) resultView() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Results"))
	b.WriteString("\n\n")
	for _, r := range m.results {
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
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("press q/enter to quit"))
	b.WriteString("\n")
	return b.String()
}

func main() {
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
