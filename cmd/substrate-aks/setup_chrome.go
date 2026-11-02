package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func (m *setupWorkspace) View() tea.View {
	st := m.st
	header := m.headerView()
	if m.width < 40 || m.height < 14 {
		content := header + "\n" + m.step + "\nResize to at least 40x14\nInput paused; Ctrl-C stops work."
		lines := strings.Split(ansi.Hardwrap(content, m.width, true), "\n")
		view := tea.NewView(strings.Join(lines[:min(len(lines), m.height)], "\n"))
		view.AltScreen = true
		return view
	}
	if m.welcome {
		content := lipgloss.Place(m.width, m.height-2, lipgloss.Center, lipgloss.Center, m.welcomeView()) +
			"\n\n" + lipgloss.PlaceHorizontal(m.width, lipgloss.Center, m.footerView())
		view := tea.NewView(content)
		view.AltScreen = true
		return view
	}
	lines := strings.Split(header, "\n")
	bodyHeight := max(0, m.height-len(lines)-3)
	body := m.panelView(bodyHeight)
	if m.width >= 96 {
		separator := strings.TrimSuffix(strings.Repeat("  "+st.muted.Render("│")+"   \n", max(1, bodyHeight)), "\n")
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.rail(), separator, body)
	}
	if m.confirmStop {
		body = setupOverlay(body, m.stopDialog(), m.width, bodyHeight)
	}
	rule := st.muted.Render(strings.Repeat("─", m.width))
	lines = append(lines, rule)
	bodyLines := strings.Split(body, "\n")
	for len(bodyLines) < bodyHeight {
		bodyLines = append(bodyLines, "")
	}
	lines = append(lines, bodyLines[:min(len(bodyLines), bodyHeight)]...)
	lines = append(lines, rule, m.footerView())
	if len(lines) > m.height {
		lines = lines[:m.height]
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], m.width, "")
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}

func (m *setupWorkspace) panelView(bodyHeight int) string {
	st := m.st
	width := m.bodyWidth()
	title, right := m.title, ""
	switch m.view {
	case "":
		if !m.workStarted.IsZero() {
			end := m.clock()
			if m.finished {
				end = m.finishedAt
			}
			right = st.muted.Render("elapsed " + setupDuration(end.Sub(m.workStarted)))
			// Timers are frozen while a question waits; say so when there is room.
			if paused := st.accent.Render("paused for input") + st.muted.Render(" · "); m.waiting() &&
				width-ansi.StringWidth(title)-ansi.StringWidth(right)-ansi.StringWidth(paused) >= 2 {
				right = paused + right
			}
		}
	case "activity":
		title, right = "LOGS", st.muted.Render("esc to return")
	default:
		title, right = strings.ToUpper(m.view), st.muted.Render("esc to return")
	}
	titleStyle := st.bold
	if m.finished && m.view == "" {
		titleStyle = st.ok.Bold(true)
		if m.err != nil {
			titleStyle = st.fail.Bold(true)
		}
	}
	panel := setupAlign(titleStyle.Render(title), right, width)
	gap := "\n\n"
	if m.height <= 18 {
		gap = "\n"
	}
	height := m.contextHeight()
	vp := m.contextViewport()
	hasContext := m.contextText() != ""
	if hasContext {
		panel += gap + vp.View()
		if vp.TotalLineCount() > height {
			keys, back := "pgup/pgdn", "pgup"
			if m.view == "" && m.arrowsScroll() {
				keys, back = "↑↓", "↑"
			}
			hint := fmt.Sprintf("▾ %d%% read · %s to scroll", int(vp.ScrollPercent()*100), keys)
			if vp.AtBottom() {
				hint = "▴ end · " + back + " to scroll back"
			}
			panel += "\n" + st.muted.Render(hint)
		} else {
			panel += "\n"
		}
	}
	if form := m.formView(); form != "" {
		separator := "\n"
		if !hasContext {
			separator = gap
		}
		if m.reviewRequired && !m.reviewed {
			panel += separator + st.warn.Render("Read the full summary (PgDn), then a to acknowledge.")
		} else {
			panel += separator + form
		}
	}
	if tail := m.logPreview(bodyHeight - lipgloss.Height(panel)); tail != "" {
		panel += "\n" + tail
	}
	return panel
}

// logPreview previews the end of the current command's log under a running
// checklist; l expands it into the scrollable logs view.
func (m *setupWorkspace) logPreview(rows int) string {
	if m.view != "" || len(m.checks) == 0 || m.form != nil || m.plan || m.finished || rows < 6 || m.logTail == "" {
		return ""
	}
	var tail []string
	for _, line := range strings.Split(m.logTail, "\n") {
		if strings.TrimSpace(ansi.Strip(line)) != "" {
			tail = append(tail, line)
		}
	}
	// Fill the rest of the body; the blank line and header take two rows.
	tail = tail[max(0, len(tail)-(rows-2)):]
	st, width := m.st, m.bodyWidth()
	label, hint := "── log ", " expand ──"
	fill := max(0, width-ansi.StringWidth(label)-1-ansi.StringWidth(hint)-1)
	lines := []string{"", st.muted.Render(label+strings.Repeat("─", fill)+" ") + st.key.Render("l") + st.muted.Render(hint)}
	for _, line := range tail {
		lines = append(lines, ansi.Truncate(line, width, "…"))
	}
	return strings.Join(lines, "\n")
}

func (m *setupWorkspace) contextHeight() int {
	formHeight := 0
	if form := m.formView(); form != "" {
		formHeight = lipgloss.Height(form) + 1
		if m.reviewRequired && !m.reviewed {
			formHeight = 2
		}
	}
	// Header, two rules, footer, title and scroll indicator.
	frame := lipgloss.Height(m.headerView()) + 5
	if m.height > 18 {
		frame++
	}
	return max(1, m.height-frame-formHeight)
}

func (m *setupWorkspace) headerView() string {
	st := m.st
	left := setupBadge(m.renderer)
	if m.stopping {
		left += st.warn.Render("  · Stopping owned work")
	}
	target := func(labels bool, cluster, subscription string) string {
		var fields []string
		for _, f := range [][2]string{{"cluster", cluster}, {"region", m.region}, {"subscription", subscription}} {
			value := f[1]
			if value == "" {
				value = st.muted.Render("—")
			}
			if labels {
				value = st.muted.Render(f[0]+" ") + value
			}
			fields = append(fields, value)
		}
		return strings.Join(fields, st.muted.Render(" · "))
	}
	// Prefer the subscription's display name; the GUID is the fallback.
	subscription, short := m.subscription, m.subscription
	if name := m.subscriptionNames[strings.ToLower(m.subscription)]; name != "" {
		subscription, short = name, ansi.Truncate(name, 16, "…")
	} else if len(short) > 13 {
		short = short[:8] + "…"
	}
	// The header stays one line so the frame never shifts as the target becomes known.
	room := m.width - ansi.StringWidth(left) - 3
	for _, right := range []string{target(true, m.cluster, subscription), target(true, m.cluster, short), target(false, m.cluster, short)} {
		if ansi.StringWidth(right) <= room {
			return setupAlign(left, right, m.width)
		}
	}
	if room < 12 {
		return ansi.Truncate(left, m.width, "…")
	}
	// Shorten an unusually long cluster name before the region and subscription.
	if spare := room - ansi.StringWidth(target(false, "", short)) + 1; spare >= 8 {
		return setupAlign(left, target(false, ansi.Truncate(m.cluster, spare, "…"), short), m.width)
	}
	return setupAlign(left, ansi.Truncate(target(false, m.cluster, short), room, "…"), m.width)
}

func (m *setupWorkspace) footerView() string {
	trailing := setupKeyHint{"ctrl+c", "stop"}
	var hints []setupKeyHint
	switch {
	case m.welcome:
		begin := setupKeyHint{"enter", "begin"}
		if m.choice != 0 {
			begin.label = "unavailable"
		}
		hints = []setupKeyHint{{"←→", "choose"}, begin, {"ctrl+c", "exit"}}
		if m.width < 50 {
			hints = hints[1:]
		}
		return setupKeys(m.st, m.width, hints, setupKeyHint{})
	case m.confirmStop:
		hints, trailing = []setupKeyHint{{"y", "stop setup"}, {"n", "keep going"}}, setupKeyHint{"ctrl+c", "stop now"}
	case m.finished && m.err == nil:
		hints, trailing = []setupKeyHint{{"enter", "show connection commands"}}, setupKeyHint{}
	case m.finished:
		hints, trailing = []setupKeyHint{{"enter", "exit"}, {"d", "details"}, {"l", "logs"}, {"?", "help"}}, setupKeyHint{"ctrl+c", "exit"}
	case m.view != "":
		hints = []setupKeyHint{{"esc", "return"}, {"↑↓", "scroll"}, {"pgup/pgdn", "page"}}
	case m.reviewRequired && !m.reviewed:
		read := "pgdn"
		if m.arrowsScroll() {
			read = "↓/pgdn"
		}
		hints = []setupKeyHint{{read, "read"}, {"a", "acknowledge summary"}, {"d", "details"}, {"l", "logs"}}
	case m.formAcceptsText():
		hints = []setupKeyHint{{"enter", "continue"}}
	default:
		scroll := "pgup/pgdn"
		if m.arrowsScroll() {
			scroll = "↑↓"
		}
		hints = []setupKeyHint{{"d", "details"}, {"l", "logs"}, {"?", "help"}, {scroll, "scroll"}}
	}
	return setupKeys(m.st, m.width, hints, trailing)
}

func (m *setupWorkspace) stopDialog() string {
	st := m.st
	width := min(58, m.width-4)
	text := ansi.Wrap("Running work is cancelled and setup exits. Azure resources created so far are retained.", width-6, "")
	content := st.bold.Render("Stop setup?") + "\n\n" + text + "\n\n" +
		st.key.Render("y") + " " + st.muted.Render("stop") + "    " + st.key.Render("n") + " " + st.muted.Render("keep going")
	return m.renderer.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(setupWarn)).
		Padding(1, 2).Width(width).Render(content)
}

func (m *setupWorkspace) welcomeView() string {
	st := m.st
	width := min(m.width-4, 100)
	wide := width >= 76
	// The brand may use the side margins; it is centered on its own.
	room := m.width - 2
	brandHeight := 32
	if wide {
		brandHeight = 22
	}
	brand := setupBadge(m.renderer)
	for _, candidate := range []struct {
		text   string
		height int
	}{
		{setupBrand(m.renderer, m.brandFrame), brandHeight},
		{setupStack(m.renderer, setupWordmark(m.renderer)), 25},
		{lipgloss.JoinVertical(lipgloss.Center, setupWordmark(m.renderer),
			lipgloss.JoinHorizontal(lipgloss.Top, setupOn(m.renderer), "  ", setupAKS(m.renderer))), 20},
		{setupStack(m.renderer, lipgloss.JoinVertical(lipgloss.Center,
			setupWordmark(m.renderer, "agent"), setupWordmark(m.renderer, "substrate"))), 30},
	} {
		if lipgloss.Width(candidate.text) <= room && m.height >= candidate.height {
			brand = candidate.text
			break
		}
	}
	cardWidth := width
	if wide {
		cardWidth = (width - 3) / 2
	}
	options := []struct{ key, title, status, detail string }{
		{"1", "Quick start", st.ok.Render("● Ready") + st.muted.Render(" · recommended"), "Guided defaults for a new AKS installation."},
		{"2", "Advanced configuration", st.warn.Render("◌ [Planned]") + st.muted.Render(" · cannot start"), "Guided editor. Not available yet."},
	}
	// Text below the title hangs under it rather than under the option key.
	hang := func(text string) string {
		return "   " + strings.ReplaceAll(ansi.Wrap(text, max(1, cardWidth-9), ""), "\n", "\n   ")
	}
	contents := make([]string, len(options))
	height := 0
	for i, option := range options {
		title := st.bold
		if i == m.choice {
			title = st.accentBold
		}
		contents[i] = st.muted.Render(option.key) + "  " + title.Render(option.title) + "\n" + hang(option.status)
		if wide || m.height >= 24 {
			contents[i] += "\n\n" + hang(st.muted.Render(option.detail))
		}
		height = max(height, lipgloss.Height(contents[i]))
	}
	var cards []string
	for i, content := range contents {
		border := st.muted
		if i == m.choice {
			border = st.accent
		}
		style := m.renderer.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border.GetForeground()).
			Width(cardWidth-2).Padding(0, 2)
		padding := 0
		if wide && m.height >= 30 {
			padding = 1
		}
		style = style.Padding(padding, 2)
		if wide {
			// Lipgloss heights include vertical padding.
			style = style.Height(height + 2*padding)
		}
		cards = append(cards, style.Render(content))
	}
	choices := strings.Join(cards, "\n")
	if wide {
		choices = lipgloss.JoinHorizontal(lipgloss.Top, cards[0], "   ", cards[1])
	}
	content := lipgloss.PlaceHorizontal(width, lipgloss.Center, brand) + "\n\n" + choices
	// The claims are a bonus: show them only when they fit above the footer.
	if m.height >= 24 {
		claims := []string{"Checks tools and Azure access first", "Previews Azure changes for approval", "Resumes saved installations by name"}
		widest := 0
		for _, claim := range claims {
			widest = max(widest, ansi.StringWidth(claim))
		}
		var facts []string
		for _, claim := range claims {
			facts = append(facts, st.ok.Render("✓")+" "+st.muted.Render(claim))
		}
		strip := strings.Join(facts, "    ")
		if ansi.StringWidth(strip) > width {
			// Pad to a common width so the stacked list centers as one aligned block.
			for i, claim := range claims {
				facts[i] += strings.Repeat(" ", widest-ansi.StringWidth(claim))
			}
			strip = strings.Join(facts, "\n")
		}
		if lipgloss.Height(content)+1+lipgloss.Height(strip) <= m.height-2 {
			content += "\n\n" + lipgloss.PlaceHorizontal(width, lipgloss.Center, strip)
		}
	}
	return content
}

func (m *setupWorkspace) rail() string {
	st := m.st
	total := len(m.phases)
	index := max(0, slices.Index(m.phases, m.phase))
	succeeded, failed := m.finished && m.err == nil, m.finished && m.err != nil
	done := 0
	for _, phase := range m.phases {
		if m.completed[phase] {
			done++
		}
	}
	label := fmt.Sprintf("STAGE %d OF %d", index+1, total)
	switch {
	case succeeded:
		label, done = "COMPLETE", total
	case failed:
		label = fmt.Sprintf("STOPPED · STAGE %d OF %d", index+1, total)
	}
	barWidth := setupRailWidth - 2
	filled := barWidth * done / max(1, total)
	bar := st.active
	if succeeded {
		bar = st.ok
	}
	header := st.muted.Render(label) + "\n" + bar.Render(strings.Repeat("━", filled)) +
		st.muted.Render(strings.Repeat("─", barWidth-filled))
	var lines []string
	for _, phase := range m.phases {
		mark, markStyle, titleStyle, timerStyle := "○", st.muted, st.muted, st.muted
		timed := true
		switch {
		case m.completed[phase] || (phase == m.phase && succeeded):
			mark, markStyle, titleStyle = "✓", st.ok, m.renderer.NewStyle()
		case phase == m.phase && failed:
			mark, markStyle, titleStyle, timerStyle = "✗", st.fail, st.fail, st.fail
		case phase == m.phase:
			mark, markStyle, titleStyle, timerStyle = "●", st.accent, st.accentBold, st.accent
		default:
			timed = false
		}
		row := markStyle.Render(mark) + " " + titleStyle.Render(setupPhaseTitle(phase))
		if start, ok := m.phaseStarted[phase]; ok && timed {
			end := m.phaseEnded[phase]
			if end.IsZero() {
				end = m.clock()
			}
			row = setupAlign(row, timerStyle.Render(setupDuration(end.Sub(start))), setupRailWidth-1)
		}
		lines = append(lines, row)
	}
	available := m.height - lipgloss.Height(m.headerView()) - 6
	gap := "\n\n"
	if len(lines)*2-1 > available {
		gap = "\n"
	}
	return m.renderer.NewStyle().Width(setupRailWidth).Render(header + "\n\n" + strings.Join(lines, gap))
}

func (m *setupWorkspace) checklistRows(checks []setupCheck) string {
	st := m.st
	width := m.bodyWidth()
	var lines []string
	for _, check := range checks {
		mark, markStyle, nameStyle, status, statusStyle := "○", st.muted, st.muted, "", st.muted
		switch {
		case check.done && check.err != nil:
			mark, markStyle, nameStyle, status, statusStyle = "✗", st.fail, st.fail, "failed", st.fail
			if errors.Is(check.err, context.Canceled) {
				mark, markStyle, nameStyle, status = "–", st.muted, st.muted, "stopped"
			}
		case check.done:
			mark, markStyle, nameStyle = "✓", st.ok, m.renderer.NewStyle()
			if !check.started.IsZero() && !check.ended.IsZero() {
				status = setupDuration(check.ended.Sub(check.started))
			}
		case !check.started.IsZero():
			mark, markStyle, nameStyle = m.checkSpinner.View(), st.active, st.activeBold
			status, statusStyle = setupDuration(m.clock().Sub(check.started)), st.active
			if m.form != nil {
				mark, status = "?", "waiting for you"
			}
		}
		row := markStyle.Render(mark) + " " + nameStyle.Render(check.name)
		lines = append(lines, setupAlign(row, statusStyle.Render(status), width))
		if m.plan && check.name == m.planCheck {
			lines = append(lines, "", m.planCallout(), "")
			continue
		}
		if !check.started.IsZero() && !m.finished &&
			((!check.done && m.form == nil) || check.err != nil) {
			detail := m.step
			if check.err != nil {
				detail = check.err.Error()
			}
			detail = safeSetupText(strings.Join(strings.Fields(detail), " "))
			if detail == "" {
				continue
			}
			const prefix = "  └─ "
			room := max(1, width-ansi.StringWidth(prefix))
			if check.err == nil {
				lines = append(lines, st.soft.Render(prefix+ansi.Truncate(detail, room, "…")))
				continue
			}
			// Failures keep up to three lines; the full text stays in logs.
			wrapped := strings.Split(ansi.Wrap(detail, room, ""), "\n")
			if len(wrapped) > 3 {
				wrapped = append(wrapped[:2], ansi.Truncate(wrapped[2]+" "+wrapped[3], room, "…"))
			}
			for i, part := range wrapped {
				if i == 0 {
					part = prefix + part
				} else {
					part = strings.Repeat(" ", ansi.StringWidth(prefix)) + part
				}
				lines = append(lines, st.fail.Render(part))
			}
		}
	}
	return strings.Join(lines, "\n")
}

// planCallout frames an inline preview under its checklist row.
func (m *setupWorkspace) planCallout() string {
	content := m.st.bold.Render(m.planTitle) + "\n\n" + strings.TrimSpace(m.body)
	return m.renderer.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).
		BorderForeground(lipgloss.Color(setupAccent)).PaddingLeft(1).MarginLeft(2).
		Width(max(1, m.bodyWidth()-3)).Render(content)
}

// failureView is the terminal error callout: what stopped, the full reported
// cause and where diagnostics live.
func (m *setupWorkspace) failureView() string {
	st := m.st
	width := max(1, m.bodyWidth()-2)
	lines := strings.Split(strings.TrimSpace(m.body), "\n")
	content := st.fail.Bold(true).Render("✗ Setup stopped during "+setupPhaseTitle(m.phase)) + "\n" +
		ansi.Wrap(lines[0], width-2, "")
	if rest := strings.TrimSpace(strings.Join(lines[1:], "\n")); rest != "" {
		content += "\n\n" + ansi.Wrap(rest, width-2, "")
	}
	if m.logPath != "" {
		content += "\n\n" + st.muted.Render("Diagnostics: ") + m.logPath
	}
	return m.renderer.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).
		BorderForeground(lipgloss.Color(setupFail)).PaddingLeft(1).Width(width).Render(content)
}

func (m *setupWorkspace) successView() string {
	st := m.st
	var rows []string
	subscription := formatAzureID(m.subscriptionNames[strings.ToLower(m.subscription)], m.subscription)
	for _, field := range [][2]string{{"Cluster", m.cluster}, {"Region", m.region}, {"Subscription", subscription},
		{"Elapsed", setupDuration(m.finishedAt.Sub(m.workStarted))}} {
		if field[1] != "" && !(field[0] == "Elapsed" && m.workStarted.IsZero()) {
			rows = append(rows, st.muted.Width(14).Render(field[0])+field[1])
		}
	}
	return st.ok.Bold(true).Render("✓ Substrate is running on AKS") + "\n\n" + strings.Join(rows, "\n") + "\n\n" +
		st.muted.Render(ansi.Wrap("Connection commands print in this shell when this screen closes.", m.bodyWidth(), ""))
}
