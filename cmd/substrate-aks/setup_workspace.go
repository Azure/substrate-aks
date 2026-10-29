package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/Azure/substrate-aks/internal/installer"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

type setupFinishedMsg struct{ err error }
type setupCancelledMsg struct{}
type setupTickMsg time.Time

const (
	setupRailWidth = 25
	setupPaneGap   = 6
)

type setupCheck struct {
	name    string
	started time.Time
	done    bool
	err     error
}

type setupFormEvent struct {
	owner *huh.Form
	msg   tea.Msg
}

// Huh navigation is asynchronous. A completed form can still have commands in
// flight; they must never submit the next form (especially an approval).
func setupFormCommand(owner *huh.Form, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		msg := cmd()
		if msg == nil {
			return nil
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			wrapped := make(tea.BatchMsg, len(batch))
			for i, child := range batch {
				wrapped[i] = setupFormCommand(owner, child)
			}
			return wrapped
		}
		return setupFormEvent{owner, msg}
	}
}

type setupWorkspace struct {
	session              *setupSession
	cancel               context.CancelFunc
	work                 func() error
	renderer             *lipgloss.Renderer
	width, height        int
	started, stepStarted time.Time
	title, body, step    string
	target               string
	cluster, region      string
	subscription         string
	phase                string
	phases               []string
	completed            map[string]bool
	steps, activity      []string
	warnings             []string
	details, logPath     string
	review               string
	plan                 bool
	view                 string
	scroll               int
	form                 *huh.Form
	reply                chan error
	welcome              bool
	choice               int
	stopping, finished   bool
	err                  error
	reviewRequired       bool
	reviewed             bool
	followActivity       bool
	checks               []setupCheck
	checklistTitle       string
	checkSpinner         spinner.Model
}

func newSetupWorkspace(s *setupSession, cancel context.CancelFunc, work func() error, out io.Writer) *setupWorkspace {
	return &setupWorkspace{
		session: s, cancel: cancel, work: work, renderer: newConsole(out).renderer,
		width: 80, height: 24, started: time.Now(), title: "Choose your installation",
		completed: map[string]bool{}, welcome: true,
	}
}

func runSetupWorkspace(ctx context.Context, o setupOptions, in io.Reader, out, terminal io.Writer, execute commandRunner) error {
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	session := &setupSession{ctx: workCtx, events: make(chan any, 64)}
	workCtx = context.WithValue(workCtx, setupSessionKey{}, session)
	var receipt bytes.Buffer
	var workErr error
	workDone := make(chan struct{})
	model := newSetupWorkspace(session, cancel, func() error {
		defer close(workDone)
		workErr = setupFlow(workCtx, o, in, &receipt, terminal, execute)
		// Present all queued operation results before displaying the final outcome.
		synced := make(chan struct{})
		session.send(setupSyncMsg(synced))
		select {
		case <-synced:
		case <-workCtx.Done():
		}
		return workErr
	}, terminal)
	// interruptContext owns OS signals; the root handles raw Ctrl-C keys.
	options := []tea.ProgramOption{
		tea.WithInput(in), tea.WithOutput(terminal), tea.WithoutSignalHandler(),
		tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
			// Embedded forms already receive the root's initial and resize events.
			if msg == tea.RequestWindowSize() {
				return nil
			}
			return msg
		}),
	}
	if colorProfile(terminal) == termenv.Ascii {
		options = append(options, tea.WithColorProfile(colorprofile.NoTTY))
	}
	_, err := tea.NewProgram(model, options...).Run()
	cancel()
	if !model.welcome {
		<-workDone
	}
	if err != nil {
		return errors.Join(err, workErr)
	}
	if model.err != nil {
		return model.err
	}
	if workErr != nil {
		return workErr
	}
	_, err = io.Copy(out, &receipt)
	return err
}

func (m *setupWorkspace) Init() tea.Cmd {
	return tea.Batch(m.waitEvent(), m.tick())
}

func (m *setupWorkspace) waitEvent() tea.Cmd {
	return func() tea.Msg {
		select {
		case event := <-m.session.events:
			return event
		case <-m.session.ctx.Done():
			return setupCancelledMsg{}
		}
	}
}

func (m *setupWorkspace) tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return setupTickMsg(t) })
}

func (m *setupWorkspace) appendActivity(text string) {
	m.activity = append(m.activity, boundedSetupText(safeSetupText(text), 4000))
	if len(m.activity) > 160 {
		m.activity = m.activity[len(m.activity)-160:]
	}
	if m.view == "activity" && m.followActivity {
		m.scroll = 1 << 20
	}
}

func (m *setupWorkspace) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if event, ok := msg.(setupFormEvent); ok {
		if event.owner != m.form {
			return m, nil
		}
		msg = event.msg
	}
	var next tea.Cmd
	switch event := msg.(type) {
	case spinner.TickMsg:
		if m.form != nil {
			return m, nil
		}
		for _, check := range m.checks {
			if !check.started.IsZero() && !check.done {
				var cmd tea.Cmd
				m.checkSpinner, cmd = m.checkSpinner.Update(event)
				return m, cmd
			}
		}
		return m, nil
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, event.Width), max(1, event.Height)
		m.sizeForm()
		if m.form != nil {
			_, cmd := m.form.Update(event)
			return m, setupFormCommand(m.form, cmd)
		}
		return m, nil
	case setupTickMsg:
		return m, m.tick()
	case setupSyncMsg:
		close(event)
		next = m.waitEvent()
	case setupCancelledMsg:
		m.stopping = true
		m.cancel()
		if m.welcome || m.finished {
			if m.err == nil {
				m.err = context.Canceled
			}
			return m, tea.Quit
		}
		return m, nil
	case setupFinishedMsg:
		m.finished, m.err = true, event.err
		for i := range m.checks {
			check := &m.checks[i]
			if event.err != nil && !check.started.IsZero() && !check.done {
				check.done, check.err = true, event.err
			}
		}
		m.form, m.reply = nil, nil
		m.step, m.steps = "", nil
		m.view, m.scroll = "", 0
		if event.err == nil || m.stopping {
			return m, tea.Quit
		}
		m.title = "Installation stopped"
		m.body = safeSetupText(event.err.Error())
		return m, nil
	case setupSectionMsg:
		m.plan = false
		m.checks = nil
		m.title, m.body, m.details = safeSetupText(string(event)), "", ""
		m.step, m.steps, m.scroll = "", nil, 0
		m.reviewRequired, m.reviewed = false, false
		next = m.waitEvent()
	case setupChecklistMsg:
		m.plan = false
		m.title = safeSetupText(event.title)
		m.checklistTitle = m.title
		m.body, m.details, m.step = "", "", ""
		m.steps, m.checks = nil, nil
		m.scroll = 0
		m.reviewRequired, m.reviewed = false, false
		for _, name := range event.items {
			m.checks = append(m.checks, setupCheck{name: safeSetupText(name)})
		}
		next = m.waitEvent()
	case setupCheckMsg:
		for i := range m.checks {
			check := &m.checks[i]
			if check.name != event.name {
				continue
			}
			if m.plan && (!event.done || event.err != nil) {
				m.plan = false
				m.title, m.body, m.details = m.checklistTitle, "", ""
				m.reviewRequired, m.reviewed = false, false
				m.scroll = 0
			}
			check.done, check.err = event.done, event.err
			if !event.done {
				check.started = time.Now()
				m.step = ""
				m.checkSpinner = spinner.New(spinner.WithSpinner(spinner.MiniDot))
				next = m.checkSpinner.Tick
			}
			status := "Started"
			if event.done {
				status = "Ready"
				if event.err != nil {
					status = event.err.Error()
				}
			}
			m.appendActivity(check.name + ": " + status)
			break
		}
		next = tea.Batch(m.waitEvent(), next)
	case setupNoteMsg:
		text := safeSetupText(event.text)
		if event.warning {
			text = "WARNING: " + text
			if m.phase == "" {
				m.warnings = append(m.warnings, boundedSetupText(text, 4000))
				if len(m.warnings) > 10 {
					m.warnings = m.warnings[len(m.warnings)-10:]
				}
			}
		}
		m.appendActivity(text)
		if m.plan && event.warning {
			m.body += "\n\n" + text
		} else if !m.plan && (len(m.checks) == 0 || event.warning) {
			m.body = boundedSetupText(m.body+"\n"+text, 12000)
		}
		next = m.waitEvent()
	case setupStepMsg:
		if len(m.checks) == 0 && m.step != "" {
			m.steps = append(m.steps, m.step)
			if len(m.steps) > 5 {
				m.steps = m.steps[len(m.steps)-5:]
			}
		}
		m.step, m.stepStarted = safeSetupText(string(event)), time.Now()
		if m.view == "" {
			m.scroll = 0
		}
		m.appendActivity(m.step)
		next = m.waitEvent()
	case setupReviewMsg:
		m.plan = false
		m.checks = nil
		m.step, m.steps = "", nil
		m.title, m.body, m.details = "Review installation", m.reviewView(event), ""
		warning := m.renderer.NewStyle().Foreground(lipgloss.Color("3")).Width(m.bodyWidth())
		if len(m.warnings) > 0 {
			m.body = warning.Render(strings.Join(m.warnings, "\n\n")) + "\n\n" + m.body
			m.warnings = nil
		}
		m.body = warning.Render("Billable ACR, AKS and storage resources.\nFailures retain resources; teardown deletes installation data.") + "\n\n" + m.body
		m.review = m.body
		m.scroll = 0
		next = m.waitEvent()
	case setupTargetMsg:
		c := event.config
		m.cluster, m.region, m.subscription = safeSetupText(c.AKS.ClusterName), safeSetupText(c.Azure.Location), safeSetupText(c.Azure.SubscriptionID)
		m.target = safeSetupText(c.AKS.ClusterName + " / " + c.Azure.Location + " / " + c.Azure.SubscriptionID)
		m.phases = nil
		if c.Substrate.Images == (installer.ImagesConfig{}) {
			m.phases = append(m.phases, "registry")
		}
		m.phases = append(m.phases, "images", "cluster", "substrate")
		if event.demo {
			m.phases = append(m.phases, "demo")
		}
		m.phases = append(m.phases, "connect")
		next = m.waitEvent()
	case setupPhaseMsg:
		phase := string(event)
		if m.phase != "" && phase != m.phase {
			m.completed[m.phase] = true
		}
		m.phase = phase
		next = m.waitEvent()
	case setupLogMsg:
		m.logPath = safeSetupText(string(event))
		m.appendActivity("Diagnostic log: " + m.logPath)
		next = m.waitEvent()
	case setupPlanMsg:
		m.plan = true
		m.title = "Review infrastructure changes"
		if m.phase == "registry" {
			m.title = "Review registry changes"
		}
		m.body = m.planView(event.changes)
		m.step, m.steps = "", nil
		m.details = safeSetupText(fullSetupPlan(event.subscription, event.changes))
		m.scroll = 0
		m.reviewRequired, m.reviewed = false, false
		for _, change := range event.changes {
			if change.ChangeType == "Delete" || (change.ChangeType != "Create" && change.ChangeType != "Modify" && change.ChangeType != "Deploy" && change.ChangeType != "NoChange") {
				m.reviewRequired = true
			}
		}
		next = m.waitEvent()
	case setupFormMsg:
		m.form, m.reply = event.form, event.done
		m.view, m.scroll = "", 0
		if len(m.checks) > 0 && !m.plan {
			m.scroll = 1 << 20
		}
		theme := huh.ThemeFunc(setupFormTheme)
		if m.renderer.ColorProfile() == termenv.Ascii {
			theme = huh.ThemeFunc(huh.ThemeBase)
		}
		m.form.WithShowHelp(false).WithTheme(theme)
		m.sizeForm()
		next = tea.Batch(m.waitEvent(), m.form.Init())
	case tea.PasteMsg:
		if m.view != "" || m.width < 40 || m.height < 14 || (m.reviewRequired && !m.reviewed) {
			return m, nil
		}
	case tea.KeyPressMsg:
		key := event.String()
		if key == "ctrl+c" {
			m.stopping = true
			m.cancel()
			if m.welcome || m.finished {
				if m.err == nil {
					m.err = context.Canceled
				}
				return m, tea.Quit
			}
			return m, nil
		}
		if m.stopping {
			return m, nil
		}
		if m.width < 40 || m.height < 14 {
			return m, nil
		}
		if m.welcome {
			return m, m.welcomeKey(key)
		}
		if m.finished && (key == "enter" || key == "esc" || key == "q") {
			return m, tea.Quit
		}
		// Global shortcuts never steal characters from an active text field.
		switch key {
		case "f2":
			m.toggleView("details")
			return m, nil
		case "f3":
			m.toggleView("activity")
			return m, nil
		case "f1":
			m.toggleView("help")
			return m, nil
		case "f4":
			if m.reviewRequired && m.view == "" {
				vp := m.contextViewport()
				if vp.AtBottom() {
					m.reviewed, m.scroll = true, 0
				}
			}
			return m, nil
		case "pgdown":
			m.scrollBy(max(1, m.contextHeight()-1))
			return m, nil
		case "pgup":
			m.scrollBy(-max(1, m.contextHeight()-1))
			return m, nil
		}
		if m.view != "" {
			switch key {
			case "esc":
				m.view, m.scroll = "", 0
			case "down", "j":
				m.scrollBy(1)
			case "up", "k":
				m.scrollBy(-1)
			case "end", "G":
				m.followActivity = true
				m.scroll = 1 << 20
			case "home", "g":
				m.followActivity = false
				m.scroll = 0
			}
			return m, nil
		}
		if m.form != nil && m.reviewRequired && !m.reviewed {
			// Destructive/unknown plans must be read through before approval.
			return m, nil
		}
	}
	if m.form != nil && !m.stopping {
		_, cmd := m.form.Update(msg)
		if m.form.State == huh.StateCompleted || m.form.State == huh.StateAborted {
			var err error
			if m.form.State == huh.StateAborted {
				err = errSetupFormAborted
			}
			m.reply <- err
			m.form, m.reply = nil, nil
			return m, next
		}
		return m, tea.Batch(next, setupFormCommand(m.form, cmd))
	}
	return m, next
}

func (m *setupWorkspace) welcomeKey(key string) tea.Cmd {
	switch key {
	case "down", "right", "j":
		m.choice = min(1, m.choice+1)
	case "up", "left", "k":
		m.choice = max(0, m.choice-1)
	case "enter":
		if m.choice != 0 {
			return nil
		}
		m.welcome = false
		m.title, m.body = "Check your setup", "Checking local tools before Azure discovery."
		work := m.work
		result := make(chan error, 1)
		go func() { result <- work() }()
		return func() tea.Msg { return setupFinishedMsg{<-result} }
	}
	return nil
}

func (m *setupWorkspace) toggleView(view string) {
	if m.view == view {
		m.view = ""
	} else {
		m.view = view
	}
	m.scroll = 0
	if m.view == "activity" {
		m.scroll, m.followActivity = 1<<20, true
	}
}

func (m *setupWorkspace) scrollBy(delta int) {
	vp := m.contextViewport()
	m.scroll = max(0, vp.YOffset()+delta)
	if delta < 0 {
		m.followActivity = false
	}
}

func (m *setupWorkspace) bodyWidth() int {
	if m.width >= 96 && len(m.phases) > 0 {
		return m.width - setupRailWidth - setupPaneGap - 2
	}
	return max(1, m.width-4)
}

func (m *setupWorkspace) sizeForm() {
	if m.form != nil {
		m.form.WithWidth(m.bodyWidth()).WithHeight(max(4, min(7, m.height-9)))
	}
}

func (m *setupWorkspace) formView() string {
	if m.form == nil || m.view != "" || m.stopping {
		return ""
	}
	return m.form.View()
}

func (m *setupWorkspace) contextHeight() int {
	formHeight := 0
	if form := m.formView(); form != "" && (!m.reviewRequired || m.reviewed) {
		formHeight = lipgloss.Height(form) + 1
	}
	frameHeight := 9
	if m.height <= 18 {
		frameHeight = 8
	}
	frameHeight += lipgloss.Height(m.headerView()) - 2
	return max(1, m.height-frameHeight-formHeight)
}

func (m *setupWorkspace) contextViewport() viewport.Model {
	vp := viewport.New(viewport.WithWidth(m.bodyWidth()), viewport.WithHeight(m.contextHeight()))
	vp.SoftWrap = true
	vp.SetContent(ansi.Hardwrap(m.contextText(), m.bodyWidth(), true))
	vp.SetHeight(max(1, min(vp.Height(), vp.TotalLineCount())))
	vp.SetYOffset(m.scroll)
	return vp
}

func (m *setupWorkspace) contextText() string {
	switch m.view {
	case "help":
		return "F2: review installation or full resource changes\nF3: recent operation activity and diagnostic log path\nPgUp/PgDn: scroll review or activity\nEsc: close details/activity; never skips an approval\nCtrl-C: stop owned work; resources are retained\n\nAdvanced guided configuration is planned, not implemented.\nExisting deploy.json configuration remains supported.\nRaw subprocess diagnostics stay in private logs; activity is a bounded list of reported operations."
	case "activity":
		return strings.Join(m.activity, "\n")
	case "details":
		if m.details != "" {
			return m.details
		}
		return m.review + "\n\n" + m.body
	default:
		if m.plan {
			return m.body
		}
		if len(m.checks) > 0 {
			return m.checklistView()
		}
		var b strings.Builder
		if m.form == nil {
			if m.step != "" {
				fmt.Fprintf(&b, "[work] %s (%s)\n\n", m.step, time.Since(m.stepStarted).Round(time.Second))
			}
			for _, step := range m.steps {
				b.WriteString("\n  - " + step)
			}
			b.WriteString("\n\n")
		}
		b.WriteString(strings.TrimSpace(m.body))
		if m.logPath != "" && m.form == nil {
			b.WriteString("\n\nDiagnostics: " + m.logPath)
		}
		return b.String()
	}
}

func (m *setupWorkspace) checklistView() string {
	var lines []string
	for _, check := range m.checks {
		mark, status, color := "○", "Pending", lipgloss.Color("8")
		switch {
		case check.done && check.err != nil:
			mark, status, color = "✗", "Failed", lipgloss.Color("1")
			if errors.Is(check.err, context.Canceled) {
				mark, status = "–", "Stopped"
			}
		case check.done:
			mark, status, color = "✓", "Ready", lipgloss.Color("2")
		case !check.started.IsZero():
			mark, status, color = m.checkSpinner.View(), time.Since(check.started).Round(time.Second).String(), lipgloss.Color(setupAccent)
			if m.form != nil {
				mark, status = "?", "Choose below"
			}
		}
		line := mark + " " + check.name + "  " + status
		lines = append(lines, m.renderer.NewStyle().Foreground(color).Render(line))
		if !check.started.IsZero() && !m.finished &&
			((!check.done && m.form == nil) || check.err != nil) {
			detail := m.step
			if check.err != nil {
				detail = check.err.Error()
			}
			detail = safeSetupText(strings.Join(strings.Fields(detail), " "))
			detail = "  " + ansi.Truncate(detail, max(1, m.bodyWidth()-2), "…")
			lines = append(lines, m.renderer.NewStyle().Foreground(lipgloss.Color("8")).Render(detail))
		}
	}
	if m.body != "" {
		lines = append(lines, "", m.body)
	}
	return strings.Join(lines, "\n")
}

func (m *setupWorkspace) reviewView(entries []setupReviewEntry) string {
	var lines []string
	width := m.bodyWidth()
	muted := m.renderer.NewStyle().Foreground(lipgloss.Color("8"))
	for _, entry := range entries {
		text, label := safeSetupText(entry.text), safeSetupText(entry.label)
		switch entry.kind {
		case "heading":
			lines = append(lines, "", m.renderer.NewStyle().Bold(true).Foreground(lipgloss.Color(setupAccent)).Width(width).Render(text))
		case "warning":
			lines = append(lines, m.renderer.NewStyle().Foreground(lipgloss.Color("3")).Width(width).Render("WARNING: "+text))
		case "success":
			lines = append(lines, m.renderer.NewStyle().Foreground(lipgloss.Color("2")).Width(width).Render(text))
		case "row":
			if width < 64 {
				lines = append(lines, muted.Render(label), m.renderer.NewStyle().PaddingLeft(2).Width(width).Render(text))
			} else {
				lines = append(lines, lipgloss.JoinHorizontal(lipgloss.Top,
					muted.Width(20).Render(label), m.renderer.NewStyle().Width(width-20).Render(text)))
			}
		default:
			lines = append(lines, muted.Width(width).Render(text))
		}
	}
	return strings.Join(lines, "\n")
}

func (m *setupWorkspace) View() tea.View {
	accent := m.renderer.NewStyle().Bold(true).Foreground(lipgloss.Color(setupAccent))
	muted := m.renderer.NewStyle().Foreground(lipgloss.Color("8"))
	header := m.headerView()
	if m.width < 40 || m.height < 14 {
		content := header + "\n" + m.step + "\nResize to at least 40x14\nInput paused; Ctrl-C stops work."
		lines := strings.Split(ansi.Hardwrap(content, m.width, true), "\n")
		view := tea.NewView(strings.Join(lines[:min(len(lines), m.height)], "\n"))
		view.AltScreen = true
		return view
	}
	title := m.title
	if m.view != "" {
		title = strings.ToUpper(m.view)
	}
	var body string
	footer := "Ctrl-C stop  F2 details  F3 activity  PgUp/PgDn scroll  F1 help"
	if m.welcome {
		body = m.welcomeView()
		footer = "Ctrl-C exit  Arrows choose  Enter begin"
		if m.choice != 0 {
			footer = "Ctrl-C exit  Arrows choose  Unavailable"
		}
		content := lipgloss.Place(m.width, m.height-2, lipgloss.Center, lipgloss.Center, body) +
			"\n\n" + lipgloss.PlaceHorizontal(m.width, lipgloss.Center, muted.Render(footer))
		view := tea.NewView(content)
		view.AltScreen = true
		return view
	} else {
		height := m.contextHeight()
		vp := m.contextViewport()
		gap := "\n\n"
		if m.height <= 18 {
			gap = "\n"
		}
		panel := accent.Render(title) + gap + vp.View()
		if vp.TotalLineCount() > height {
			panel += "\n" + muted.Render(fmt.Sprintf("Review %d%% - PgUp/PgDn", int(vp.ScrollPercent()*100)))
		} else {
			panel += "\n"
		}
		if form := m.formView(); form != "" {
			if m.reviewRequired && !m.reviewed {
				panel += "\nRead the full summary (PgDn), then F4 to acknowledge."
			} else {
				panel += "\n" + form
			}
		}
		if m.width >= 96 && len(m.phases) > 0 {
			body = lipgloss.JoinHorizontal(lipgloss.Top, m.rail(), strings.Repeat(" ", setupPaneGap), panel)
		} else {
			body = panel
		}
		if m.finished {
			footer = "PgUp/PgDn review  Enter exit  F3 activity"
		} else if m.view != "" {
			footer = "Esc return  Up/Down scroll  PgUp/PgDn page  Ctrl-C stop"
		} else if m.reviewRequired && !m.reviewed {
			footer = "Ctrl-C stop  PgDn review  F4 acknowledge summary  F2 details"
		}
	}
	rule := muted.Render(strings.Repeat("─", m.width))
	lines := append(strings.Split(header, "\n"), rule)
	bodyLines := strings.Split(body, "\n")
	bodyHeight := max(0, m.height-len(lines)-2)
	for len(bodyLines) < bodyHeight {
		bodyLines = append(bodyLines, "")
	}
	lines = append(lines, bodyLines[:min(len(bodyLines), bodyHeight)]...)
	lines = append(lines, rule, footer)
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

func (m *setupWorkspace) headerView() string {
	brand := m.renderer.NewStyle().Bold(true).Foreground(lipgloss.Color(setupAccent)).Render("Agent Substrate on AKS")
	muted := m.renderer.NewStyle().Foreground(lipgloss.Color("8"))
	if m.stopping {
		brand += "\n" + muted.Render("Stopping owned work")
	}
	if m.width < 80 || m.height < 20 || m.subscription == "" {
		target := m.target
		if target == "" {
			target = "Guided installation / no Azure resources changed yet"
		}
		return brand + "\n" + muted.Render(ansi.Truncate(target, m.width, "…"))
	}
	valueWidth := max(1, m.width-lipgloss.Width(brand)-4-14)
	var info []string
	for _, field := range [][2]string{{"Cluster", m.cluster}, {"Region", m.region}, {"Subscription", m.subscription}} {
		info = append(info, muted.Width(14).Render(field[0])+ansi.Truncate(field[1], valueWidth, "…"))
	}
	target := strings.Join(info, "\n")
	gap := max(4, m.width-lipgloss.Width(brand)-lipgloss.Width(target))
	return lipgloss.JoinHorizontal(lipgloss.Center, brand, strings.Repeat(" ", gap), target)
}

func (m *setupWorkspace) welcomeView() string {
	width := min(m.width-4, 104)
	wide := width >= 76
	accent := m.renderer.NewStyle().Bold(true).Foreground(lipgloss.Color(setupAccent))
	muted := m.renderer.NewStyle().Foreground(lipgloss.Color("8"))
	brand := accent.Render("SUBSTRATE on AKS")
	if width >= 64 && ((wide && m.height >= 24) || m.height >= 34) {
		brand = setupBrand(m.renderer)
	} else if m.height >= 26 {
		brand += "\n" + muted.Render(ansi.Wrap("An AKS packaging layer for Agent Substrate.", width, ""))
	}
	cardWidth := width
	if wide {
		cardWidth = (width - 3) / 2
	}
	options := []struct{ title, status, detail string }{
		{"Quick start", "Ready to configure", "Guided defaults for a new AKS installation."},
		{"Advanced configuration", "[Planned] - cannot start", "Guided editor. Not available yet."},
	}
	var cards []string
	for i, option := range options {
		color := lipgloss.Color("8")
		if i == m.choice {
			color = lipgloss.Color(setupAccent)
		}
		style := m.renderer.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(color).
			Width(cardWidth-2).Padding(0, 2)
		title := m.renderer.NewStyle().Bold(i == m.choice).Foreground(color).Render(option.title)
		content := title + "\n" + muted.Render(option.status)
		if wide || m.height >= 24 {
			style = style.Height(5)
			content += "\n\n" + ansi.Wrap(option.detail, cardWidth-6, "")
		}
		if wide && m.height >= 30 {
			style = style.Padding(1, 2).Height(7)
		}
		cards = append(cards, style.Render(content))
	}
	choices := strings.Join(cards, "\n")
	if wide {
		choices = lipgloss.JoinHorizontal(lipgloss.Top, cards[0], "   ", cards[1])
	}
	content := lipgloss.PlaceHorizontal(width, lipgloss.Center, brand) + "\n\n" + choices
	if m.height >= 24 {
		content += "\n\n" + lipgloss.PlaceHorizontal(width, lipgloss.Center,
			ansi.Wrap("Saved deploy.json settings are honored.", width, ""))
	}
	return content
}

func (m *setupWorkspace) rail() string {
	var lines []string
	for _, phase := range m.phases {
		title := setupPhaseTitle(phase)
		style := m.renderer.NewStyle().Foreground(lipgloss.Color("8"))
		if m.completed[phase] {
			title, style = "✓ "+title, m.renderer.NewStyle().Foreground(lipgloss.Color("2"))
		} else if phase == m.phase {
			style = m.renderer.NewStyle().Bold(true).Foreground(lipgloss.Color(setupAccent))
			if m.finished {
				title, style = "✗ "+title, m.renderer.NewStyle().Foreground(lipgloss.Color("1"))
			}
		}
		lines = append(lines, style.Render(title))
	}
	return m.renderer.NewStyle().Width(setupRailWidth).Render("Installation\n\n" + strings.Join(lines, "\n\n"))
}

func setupPhaseTitle(phase string) string {
	switch phase {
	case "registry":
		return "Private registry"
	case "images":
		return "Images and assets"
	case "cluster":
		return "AKS cluster"
	case "substrate":
		return "Substrate"
	case "demo":
		return "Counter demo"
	case "connect":
		return "Connect"
	default:
		return "Prepare"
	}
}

func fullSetupPlan(subscription string, changes []installer.ResourceChange) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Scope: /subscriptions/%s\n\n", subscription)
	for _, change := range changes {
		label, _ := planChange(change.ChangeType)
		fmt.Fprintf(&b, "%s\n%s\n\n", label, change.ResourceID)
	}
	if len(changes) == 0 {
		b.WriteString("No resource changes.\n")
	}
	b.WriteString("Property-level diagnostics are in the private command log.")
	return b.String()
}

func safeSetupText(text string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, ansi.Strip(text))
}

func boundedSetupText(text string, limit int) string {
	if len(text) > limit {
		const prefix = "[Earlier detail omitted; see diagnostic log]\n"
		offset := len(text) - limit + len(prefix)
		for offset < len(text) && !utf8.RuneStart(text[offset]) {
			offset++
		}
		return prefix + text[offset:]
	}
	return text
}

var _ tea.Model = (*setupWorkspace)(nil)
