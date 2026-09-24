package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var (
	currentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	cursorStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	rowBgStyle   = lipgloss.NewStyle().Background(lipgloss.Color("236")) // cursor row band
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
	title := fmt.Sprintf("diff — %s (vs %s)", m.diffBranch, base)
	if m.diffStyled {
		title += " · delta"
	}
	b.WriteString(headerStyle.Render(title))
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
		line := truncate(m.diffLines[i], m.width)
		if !m.diffStyled {
			line = colorizeDiffLine(line)
		}
		b.WriteString(line)
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
		help = "↑/↓ move · space select · a/n all/none · c checkout · / filter · r remote · v view · x gone · p prune · s sort · o order · f force · d delete · ? help · q quit"
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

	// Selection outranks the cursor: the row band already marks the cursor,
	// and a name that stays yellow under it keeps the pending delete visible.
	var nameRendered string
	switch {
	case br.selected:
		nameRendered = selStyle.Render(name)
	case br.isCurrent:
		nameRendered = currentStyle.Render(name)
	case isCursor:
		nameRendered = cursorStyle.Render(name)
	default:
		nameRendered = nameStyle.Render(name)
	}

	track := m.trackStr(br)
	abs := fmt.Sprintf("%-11s", br.committed.Format("2006-Jan-02"))
	rel := fmt.Sprintf("%-13s", br.committedRel)
	hash := fmt.Sprintf("%-8s", br.hash)

	row := fmt.Sprintf("%s%s %s %s %s  %s %s %s %s %s",
		cursor, sel, rem, cur, nameRendered, track,
		dimStyle.Render(abs), dimStyle.Render(rel), hashStyle.Render(hash),
		subjectStyle.Render(truncate(br.subject, m.subjectWidth(nameW))))
	if isCursor {
		row = highlightRow(row, m.width)
	}
	return row
}

// highlightRow paints the cursor band behind an already-styled row. Each
// column ends with a reset that would drop a background wrapped around the
// whole row, so the band is re-armed after every reset instead. The row is
// padded first so the band spans the full terminal width.
func highlightRow(row string, width int) string {
	const reset = "\x1b[0m"
	// Rendering nothing yields just the on/off sequences, or "" when the
	// color profile disables styling — then there is no band to paint.
	on, ok := strings.CutSuffix(rowBgStyle.Render(""), reset)
	if !ok || on == "" {
		return row
	}
	if d := width - ansi.StringWidth(row); d > 0 {
		row += strings.Repeat(" ", d)
	}
	return on + strings.ReplaceAll(row, reset, reset+on) + reset
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
		{"c", "checkout the branch under the cursor"},
		{"/", "filter by name (enter keep · esc clear)"},
		{"r", "toggle delete of upstream remote branch"},
		{"v", "view branch diff (through delta when installed)"},
		{"x", "select gone branches that hold no unique work"},
		{"p", "fetch --all --prune, then do the same as x"},
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
		"the default branch are left unselected by x/p and flagged on the confirm screen."))
	b.WriteString("\n\n")

	commit, date := buildInfo()
	b.WriteString(dimStyle.Render(fmt.Sprintf("build date: %s · commit: %s", date, commit)))
	b.WriteString("\n\n")

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
