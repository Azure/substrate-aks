package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
type setupCloseMsg struct{}
type setupCancelledMsg struct{}
type setupTickMsg time.Time
type setupBrandTickMsg struct{}

const (
	setupRailWidth = 27
	setupPaneGap   = 6
)

type setupCheck struct {
	name    string
	started time.Time
	ended   time.Time
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
	st                   setupStyles
	width, height        int
	started, stepStarted time.Time
	title, body, step    string
	target               string
	cluster, region      string
	subscription         string
	subscriptionNames    map[string]string
	phase                string
	phases               []string
	completed            map[string]bool
	steps, activity      []string
	activityAt           []time.Time
	workStarted          time.Time
	finishedAt           time.Time
	phaseStarted         map[string]time.Time
	phaseEnded           map[string]time.Time
	// Timers read clock(), which stands still while a question waits on the user.
	paused             time.Duration
	pauseStart         time.Time
	confirmStop        bool
	warnings           []string
	details, logPath   string
	logTail            string
	review             string
	plan               bool
	view               string
	scroll             int
	form               *huh.Form
	formResize         func(height int)
	reply              chan error
	welcome            bool
	brandFrame         int
	choice             int
	stopping, finished bool
	err                error
	reviewRequired     bool
	reviewed           bool
	followActivity     bool
	checks             []setupCheck
	checklistTitle     string
	// planCheck is the checklist row whose preview is shown inline beneath it.
	planCheck, planTitle string
	checkSpinner         spinner.Model
}

func newSetupWorkspace(s *setupSession, cancel context.CancelFunc, work func() error, out io.Writer) *setupWorkspace {
	renderer := newConsole(out).renderer
	return &setupWorkspace{
		session: s, cancel: cancel, work: work, renderer: renderer, st: newSetupStyles(renderer),
		width: 80, height: 24, started: time.Now(), title: "Choose your installation",
		completed: map[string]bool{}, welcome: true,
		phaseStarted: map[string]time.Time{}, phaseEnded: map[string]time.Time{},
		phase: "prepare", phases: setupPhases(installer.Config{}, false),
	}
}

func runSetupWorkspace(ctx context.Context, o setupOptions, in io.Reader, out, terminal io.Writer, execute commandRunner) error {
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	session := &setupSession{ctx: workCtx, events: make(chan any, 64), ui: newSetupUI(terminal), receipt: newSetupUI(out)}
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
	model.phases = setupPhases(installer.Config{}, o.withDemo)
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
	return tea.Batch(m.waitEvent(), m.tick(), m.brandTick())
}

// brandTick animates the agents on the welcome mark. The mark needs 256
// colors, and the animation stops once setup begins.
func (m *setupWorkspace) brandTick() tea.Cmd {
	if !m.welcome || m.renderer.ColorProfile() > termenv.ANSI256 {
		return nil
	}
	return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return setupBrandTickMsg{} })
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
	m.activityAt = append(m.activityAt, time.Now())
	if len(m.activity) > 160 {
		m.activity = m.activity[len(m.activity)-160:]
		m.activityAt = m.activityAt[len(m.activityAt)-160:]
	}
	if m.view == "activity" && m.followActivity {
		m.scroll = 1 << 20
	}
}

// clock is the work clock used for every timer. It excludes time spent waiting
// on a question or approval, so durations measure setup work only.
func (m *setupWorkspace) clock() time.Time {
	now := time.Now()
	if m.waiting() {
		now = m.pauseStart
	}
	return now.Add(-m.paused)
}

func (m *setupWorkspace) waiting() bool { return !m.pauseStart.IsZero() }

func (m *setupWorkspace) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	model, cmd := m.update(msg)
	waiting := m.form != nil && !m.workStarted.IsZero() && !m.finished
	switch {
	case waiting && !m.waiting():
		m.pauseStart = time.Now()
	case !waiting && m.waiting():
		m.paused += time.Since(m.pauseStart)
		m.pauseStart = time.Time{}
	}
	return model, cmd
}

func (m *setupWorkspace) update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		m.scrollToCurrentCheck()
		if m.form != nil {
			_, cmd := m.form.Update(event)
			return m, setupFormCommand(m.form, cmd)
		}
		return m, nil
	case setupTickMsg:
		if m.view == "activity" || m.view == "" {
			m.refreshLog()
		}
		return m, m.tick()
	case setupBrandTickMsg:
		m.brandFrame++
		return m, m.brandTick()
	case setupCloseMsg:
		return m, tea.Quit
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
		m.finished, m.err, m.confirmStop = true, event.err, false
		m.finishedAt = m.clock()
		if m.phaseEnded[m.phase].IsZero() {
			m.phaseEnded[m.phase] = m.finishedAt
		}
		for i := range m.checks {
			check := &m.checks[i]
			if event.err != nil && !check.started.IsZero() && !check.done {
				check.done, check.err = true, event.err
			}
		}
		m.form, m.reply = nil, nil
		m.step, m.steps = "", nil
		m.view, m.scroll = "", 0
		if m.stopping {
			return m, tea.Quit
		}
		if event.err == nil {
			// Hold a moment on the success state; the receipt prints after exit.
			m.title = "Setup complete"
			return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return setupCloseMsg{} })
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
	case setupCheckGroupMsg:
		if !event.done {
			for i, check := range m.checks {
				if slices.Contains(event.names, check.name) {
					m.checks[i] = setupCheck{name: check.name}
				}
			}
		} else if event.err != nil {
			failed := -1
			for i, check := range m.checks {
				if !slices.Contains(event.names, check.name) {
					continue
				}
				if check.err != nil {
					failed = -1
					break
				}
				failed = i
				if !check.done {
					break
				}
			}
			if failed >= 0 {
				return m.Update(setupCheckMsg{name: m.checks[failed].name, done: true, err: event.err})
			}
		}
		next = m.waitEvent()
	case setupCheckMsg:
		for i := range m.checks {
			check := &m.checks[i]
			if check.name != event.name {
				continue
			}
			if !event.done && !check.done && !check.started.IsZero() {
				break
			}
			if m.plan && (!event.done || event.err != nil) {
				m.plan = false
				m.title, m.body, m.details = m.checklistTitle, "", ""
				m.reviewRequired, m.reviewed = false, false
				m.scroll = 0
			}
			check.done, check.err = event.done, event.err
			check.ended = m.clock()
			if !event.done {
				check.started, check.ended = m.clock(), time.Time{}
				m.step = ""
				m.checkSpinner = spinner.New(spinner.WithSpinner(spinner.MiniDot))
				next = m.checkSpinner.Tick
				m.scrollToCurrentCheck()
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
			if m.phase == "prepare" {
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
		m.step, m.stepStarted = safeSetupText(string(event)), m.clock()
		if m.view == "" {
			m.scroll = 0
			m.scrollToCurrentCheck()
		}
		m.appendActivity(m.step)
		next = m.waitEvent()
	case setupReviewMsg:
		m.plan = false
		m.checks = nil
		m.step, m.steps = "", nil
		m.title, m.body, m.details = "Review installation", m.reviewView(event), ""
		// Billing guidance is part of the summary's closing section, next to
		// the confirmation; only carried-over identity warnings lead.
		if len(m.warnings) > 0 {
			warning := m.st.warn.Width(m.bodyWidth())
			m.body = warning.Render(strings.Join(m.warnings, "\n\n")) + "\n\n" + m.body
			m.warnings = nil
		}
		m.review = m.body
		m.scroll = 0
		next = m.waitEvent()
	case setupTargetMsg:
		c := event.config
		m.cluster, m.region, m.subscription = safeSetupText(c.AKS.ClusterName), safeSetupText(c.Azure.Location), safeSetupText(c.Azure.SubscriptionID)
		m.target = safeSetupText(c.AKS.ClusterName + " / " + c.Azure.Location + " / " + c.Azure.SubscriptionID)
		m.phases = setupPhases(c, event.demo)
		next = m.waitEvent()
	case setupSubscriptionMsg:
		if m.subscriptionNames == nil {
			m.subscriptionNames = map[string]string{}
		}
		m.subscriptionNames[strings.ToLower(event.id)] = safeSetupText(event.name)
		next = m.waitEvent()
	case setupPhaseMsg:
		phase := string(event)
		if m.phase != "" && phase != m.phase {
			m.completed[m.phase] = true
			m.phaseEnded[m.phase] = m.clock()
		}
		if _, ok := m.phaseStarted[phase]; !ok && phase != m.phase {
			m.phaseStarted[phase] = m.clock()
		}
		m.phase = phase
		next = m.waitEvent()
	case setupLogMsg:
		m.logPath = safeSetupText(string(event))
		m.refreshLog()
		next = m.waitEvent()
	case setupPlanMsg:
		m.plan = true
		m.planTitle = "Review infrastructure changes"
		if m.phase == "images" {
			m.planTitle = "Review registry changes"
		}
		// The preview expands under its checklist row so the phase stays in view.
		m.planCheck = ""
		for _, check := range m.checks {
			if !check.started.IsZero() && !check.done {
				m.planCheck = check.name
			}
		}
		if m.planCheck == "" {
			m.title = m.planTitle
		}
		m.body = m.planView(event.changes, m.planCheck != "")
		m.step, m.steps = "", nil
		m.details = safeSetupText(fullSetupPlan(event.subscription, event.changes))
		m.scroll = 0
		m.reviewRequired, m.reviewed = false, false
		m.scrollToCurrentCheck()
		for _, change := range event.changes {
			if change.ChangeType == "Delete" || (change.ChangeType != "Create" && change.ChangeType != "Modify" && change.ChangeType != "Deploy" && change.ChangeType != "NoChange") {
				m.reviewRequired = true
			}
		}
		next = m.waitEvent()
	case setupFormMsg:
		m.form, m.reply = event.form, event.done
		m.formResize = event.resize
		m.view, m.scroll = "", 0
		if len(m.checks) > 0 && !m.plan {
			m.scrollToCurrentCheck()
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
		if m.confirmStop && (key == "y" || key == "Y") {
			key = "ctrl+c"
		}
		if key == "ctrl+c" && !m.welcome && !m.finished && !m.stopping && !m.confirmStop {
			// Stopping cancels Azure work mid-flight; ask first. A second
			// Ctrl-C (or y) stops immediately.
			m.confirmStop = true
			return m, nil
		}
		if m.confirmStop && key != "ctrl+c" {
			if key == "n" || key == "N" || key == "esc" {
				m.confirmStop = false
			}
			return m, nil
		}
		if key == "ctrl+c" {
			m.confirmStop = false
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
		if m.finished && (key == "enter" || key == "esc" || key == "q" || m.err == nil) {
			return m, tea.Quit
		}
		if m.view != "" || !m.formAcceptsText() {
			switch key {
			case "d":
				m.toggleView("details")
				return m, nil
			case "l":
				m.toggleView("activity")
				return m, nil
			case "?":
				m.toggleView("help")
				return m, nil
			case "a":
				if m.reviewRequired && m.view == "" {
					vp := m.contextViewport()
					if vp.AtBottom() {
						m.reviewed, m.scroll = true, 0
					}
				}
				return m, nil
			}
		}
		switch key {
		case "pgdown":
			m.scrollBy(max(1, m.contextHeight()-1))
			return m, nil
		case "pgup":
			m.scrollBy(-max(1, m.contextHeight()-1))
			return m, nil
		}
		if m.view == "" && m.arrowsScroll() {
			switch key {
			case "down", "j":
				m.scrollBy(1)
				return m, nil
			case "up", "k":
				m.scrollBy(-1)
				return m, nil
			}
		}
		if m.view != "" {
			switch key {
			case "esc":
				m.view, m.scroll = "", 0
				m.scrollToCurrentCheck()
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
	case "2":
		m.choice = 1
	case "enter", "1":
		if key == "1" {
			m.choice = 0
		}
		if m.choice != 0 {
			return nil
		}
		m.welcome = false
		m.workStarted = m.clock()
		m.phaseStarted[m.phase] = m.workStarted
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
		m.refreshLog()
	} else if m.view == "" {
		m.scrollToCurrentCheck()
	}
}

func (m *setupWorkspace) formAcceptsText() bool {
	if m.form == nil {
		return false
	}
	field := m.form.GetFocusedField()
	if filter, ok := field.(interface{ GetFiltering() bool }); ok {
		return filter.GetFiltering()
	}
	_, confirmation := field.(*huh.Confirm)
	return !confirmation
}

// arrowsScroll reports whether up/down are free to scroll the main pane:
// a yes/no confirmation only uses left/right, unlike selects and inputs.
func (m *setupWorkspace) arrowsScroll() bool {
	if m.form == nil {
		return false
	}
	_, confirmation := m.form.GetFocusedField().(*huh.Confirm)
	return confirmation
}

func (m *setupWorkspace) scrollBy(delta int) {
	vp := m.contextViewport()
	m.scroll = max(0, vp.YOffset()+delta)
	if delta < 0 {
		m.followActivity = false
	}
}

func (m *setupWorkspace) bodyWidth() int {
	if m.width >= 96 {
		return m.width - setupRailWidth - setupPaneGap - 2
	}
	return max(1, m.width-4)
}

func (m *setupWorkspace) sizeForm() {
	if m.form != nil {
		m.form.WithWidth(m.bodyWidth())
		height := max(4, min(7, m.height-9))
		if m.formResize != nil {
			height = m.height - lipgloss.Height(m.headerView()) - 5
			if m.height <= 18 {
				height++
			}
			height = max(4, height)
			m.formResize(height)
		}
		m.form.WithHeight(height)
	}
}

func (m *setupWorkspace) formView() string {
	if m.form == nil || m.view != "" || m.stopping {
		return ""
	}
	view := m.form.View()
	if m.arrowsScroll() {
		// huh pads every form to its fixed height; a one-line confirmation
		// gives that space back to the summary above it.
		lines := strings.Split(view, "\n")
		for len(lines) > 1 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
			lines = lines[:len(lines)-1]
		}
		view = strings.Join(lines, "\n")
	}
	return view
}

func (m *setupWorkspace) contextViewport() viewport.Model {
	vp := viewport.New(viewport.WithWidth(m.bodyWidth()), viewport.WithHeight(m.contextHeight()))
	vp.SoftWrap = true
	vp.SetContent(ansi.Wrap(m.contextText(), m.bodyWidth(), ""))
	vp.SetHeight(max(1, min(vp.Height(), vp.TotalLineCount())))
	vp.SetYOffset(m.scroll)
	return vp
}

func (m *setupWorkspace) contextText() string {
	switch m.view {
	case "help":
		var b strings.Builder
		for _, row := range [][2]string{
			{"d", "review installation or full resource changes"},
			{"l", "live tail of the current command's diagnostic log"},
			{"?", "help"},
			{"a", "acknowledge a destructive/unknown summary after reading it through"},
			{"PgUp/PgDn", "scroll reviews or logs"},
			{"↑/↓", "scroll a review while a yes/no prompt is focused"},
			{"Esc", "close details/logs; never skips an approval"},
			{"Ctrl-C", "stop owned work after confirming (twice stops now); resources are retained"},
		} {
			// Descriptions wrap beneath themselves, not beneath the key column.
			text := strings.ReplaceAll(ansi.Wrap(row[1], max(1, m.bodyWidth()-12), ""), "\n", "\n"+strings.Repeat(" ", 12))
			b.WriteString(m.st.key.Width(12).Render(row[0]) + text + "\n")
		}
		b.WriteString("\n" + m.st.muted.Render("Letter shortcuts pause while typing text or filtering choices.\nAdvanced guided configuration is planned, not implemented.\nRun setup NAME to resume a saved installation.\nThe logs view tails the private command log; the full file stays on disk."))
		return b.String()
	case "activity":
		if m.logTail == "" {
			note := "No command log yet. Reported activity so far:"
			return m.st.muted.Render(note) + "\n\n" + strings.Join(m.activity, "\n")
		}
		return m.st.muted.Render("Following "+m.logPath) + "\n\n" + m.logTail
	case "details":
		if m.details != "" {
			return m.details
		}
		return m.review + "\n\n" + m.body
	default:
		if m.plan {
			if m.planCheck != "" {
				return m.checklistRows(m.checks)
			}
			return m.body
		}
		if m.finished && m.err == nil {
			return m.successView()
		}
		if m.finished && !m.stopping && strings.TrimSpace(m.body) != "" {
			if len(m.checks) > 0 {
				return m.checklistRows(m.checks) + "\n\n" + m.failureView()
			}
			return m.failureView()
		}
		if len(m.checks) > 0 {
			return m.checklistView()
		}
		var b strings.Builder
		if m.form == nil {
			if m.step != "" {
				fmt.Fprintf(&b, "[work] %s (%s)\n\n", m.step, m.clock().Sub(m.stepStarted).Round(time.Second))
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
	text := m.checklistRows(m.checks)
	if m.body != "" {
		text += "\n\n" + m.body
	}
	return text
}

func (m *setupWorkspace) scrollToCurrentCheck() {
	if m.view != "" || (m.plan && m.planCheck == "") {
		return
	}
	for i, check := range m.checks {
		current := check.err != nil || (!check.done && !check.started.IsZero())
		if m.plan {
			current = check.name == m.planCheck
		}
		if current {
			m.scroll = 0
			if i > 0 {
				before := ansi.Wrap(m.checklistRows(m.checks[:i]), m.bodyWidth(), "")
				m.scroll = strings.Count(before, "\n") + 1
			}
			return
		}
	}
}

// reviewView lays the installation summary out as titled groups of aligned
// label/value rows, with each row's explanation under its value and the
// billing warnings in a closing box next to the approval prompt.
func (m *setupWorkspace) reviewView(entries []setupReviewEntry) string {
	var lines []string
	width := min(m.bodyWidth(), 100)
	st := m.st
	stacked := width < 64
	const labelWidth = 22
	valueWidth := max(1, width-labelWidth)
	for _, entry := range entries {
		text, label, detail := safeSetupText(entry.text), safeSetupText(entry.label), safeSetupText(entry.detail)
		switch entry.kind {
		case "heading":
			lines = append(lines, "", st.accentBold.Width(width).Render(text))
		case "group":
			rule := ""
			if fill := width - lipgloss.Width(text) - 1; fill > 0 {
				rule = " " + st.muted.Render(strings.Repeat("─", fill))
			}
			lines = append(lines, "", st.accentBold.Render(text)+rule)
		case "warning":
			lines = append(lines, st.warn.Width(width).Render("WARNING: "+text))
		case "success":
			lines = append(lines, st.ok.Bold(true).Render("✓ "+text))
			if detail != "" {
				lines = append(lines, st.muted.PaddingLeft(2).Width(width).Render(setupDisplayPath(detail)))
			}
		case "row":
			value := text
			if detail != "" {
				value += "\n" + st.muted.Render(detail)
			}
			if stacked {
				lines = append(lines, st.muted.Render(label), m.renderer.NewStyle().PaddingLeft(2).Width(width).Render(value))
			} else {
				lines = append(lines, lipgloss.JoinHorizontal(lipgloss.Top,
					st.muted.Width(labelWidth).Render(label), m.renderer.NewStyle().Width(valueWidth).Render(value)))
			}
		case "callout":
			lines = append(lines, "", m.reviewCallout(label, strings.Split(text, "\n"), detail, width))
		default:
			lines = append(lines, st.muted.Width(width).Render(text))
		}
	}
	return strings.Join(lines, "\n")
}

func (m *setupWorkspace) reviewCallout(title string, warnings []string, note string, width int) string {
	st := m.st
	boxed := width >= 40
	inner := width
	if boxed {
		inner -= 4
	}
	content := []string{st.bold.Render(title)}
	for _, warning := range warnings {
		if warning != "" {
			content = append(content, st.warn.Width(inner).Render("⚠ "+warning))
		}
	}
	if note != "" {
		content = append(content, st.muted.Width(inner).Render(note))
	}
	body := strings.Join(content, "\n")
	if !boxed {
		return body
	}
	return m.renderer.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(setupWarn)).
		Padding(0, 1).Width(width - 2).Render(body)
}

// setupDisplayPath abbreviates the home directory so long state paths fit.
func setupDisplayPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == "/" {
		return path
	}
	if path == home || strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}

func setupPhaseTitle(phase string) string {
	switch phase {
	case "prepare":
		return "Preparation"
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
