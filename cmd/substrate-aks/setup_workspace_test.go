package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/Azure/substrate-aks/internal/installer"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func testWorkspace(t *testing.T) *setupWorkspace {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	session := &setupSession{ctx: ctx, events: make(chan any, 64)}
	return newSetupWorkspace(session, cancel, func() error { return nil }, io.Discard)
}

func workspaceKey(m *setupWorkspace, code rune) tea.Cmd {
	_, cmd := m.Update(tea.KeyPressMsg{Code: code})
	return cmd
}

func TestWorkspacePlannedOptionsCannotStartInstallation(t *testing.T) {
	m := testWorkspace(t)
	for range 3 {
		workspaceKey(m, tea.KeyRight)
		if m.choice != 1 {
			t.Fatalf("navigation escaped the two-option chooser: %d", m.choice)
		}
		if cmd := workspaceKey(m, tea.KeyEnter); cmd != nil || !m.welcome {
			t.Fatal("planned option started work")
		}
		if view := m.View().Content; !strings.Contains(view, "[Planned]") || !strings.Contains(view, "cannot start") {
			t.Fatalf("planned option was not clearly unavailable:\n%s", view)
		}
	}
	workspaceKey(m, tea.KeyLeft)
	cmd := workspaceKey(m, tea.KeyEnter)
	if cmd == nil || m.welcome {
		t.Fatal("Quick start did not start work")
	}
	if result, ok := cmd().(setupFinishedMsg); !ok || result.err != nil {
		t.Fatalf("unexpected operation result: %#v", result)
	}
	m = testWorkspace(t)
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 14})
	m.choice = 1
	if view := m.View().Content; !strings.Contains(view, "Advanced configuration") || !strings.Contains(view, "cannot start") {
		t.Fatalf("narrow window hid the selected planned option:\n%s", view)
	}
}

func TestWorkspaceWelcomeCardsFitTerminal(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {100, 30}, {80, 24}, {79, 24}, {64, 34}, {50, 30}, {40, 26}, {40, 24}, {80, 14}, {40, 14}} {
		for _, choice := range []int{0, 1} {
			m := testWorkspace(t)
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			m.choice = choice
			view := ansi.Strip(m.View().Content)
			for _, want := range []string{"Quick start", "Advanced configuration", "[Planned]", "cannot start", "ctrl+c"} {
				if !strings.Contains(view, want) {
					t.Fatalf("%dx%d missing %q:\n%s", size[0], size[1], want, view)
				}
			}
			if strings.Contains(view, "Existing AKS cluster") {
				t.Fatal("removed option is still advertised")
			}
			if size[0] >= 80 && size[1] >= 24 {
				for _, want := range []string{"╭─╮ ╭─╴ ┌─╴ ┌─╮ ╶┬╴", "╭─╴ ╷ ╷ ┌─╮", "█▀▀█ █▀▀▄ ▄▄█▀", "An AKS packaging layer"} {
					if !strings.Contains(view, want) {
						t.Fatalf("landing screen lost branding %q:\n%s", want, view)
					}
				}
			}
			for _, line := range strings.Split(view, "\n") {
				if ansi.StringWidth(line) > size[0] {
					t.Fatalf("%dx%d exceeded terminal width:\n%s", size[0], size[1], view)
				}
			}
			if lines := strings.Count(view, "\n") + 1; lines > size[1] {
				t.Fatalf("%dx%d exceeded terminal height (%d):\n%s", size[0], size[1], lines, view)
			}
		}
	}
}

func TestWorkspaceAnimatesBrandOnlyOnColorWelcome(t *testing.T) {
	m := testWorkspace(t)
	if m.brandTick() != nil {
		t.Fatal("monochrome welcome animated the mark")
	}
	m.renderer.SetColorProfile(termenv.ANSI256)
	if _, cmd := m.Update(setupBrandTickMsg{}); cmd == nil || m.brandFrame != 1 {
		t.Fatalf("welcome stopped animating at frame %d", m.brandFrame)
	}
	m.welcome = false
	if _, cmd := m.Update(setupBrandTickMsg{}); cmd != nil {
		t.Fatal("brand kept animating after the welcome screen")
	}
}

func TestWorkspaceWelcomeSelectionHighlightsWithoutMovingText(t *testing.T) {
	m := testWorkspace(t)
	m.renderer.SetColorProfile(termenv.TrueColor)
	first := m.welcomeView()
	workspaceKey(m, tea.KeyRight)
	second := m.welcomeView()
	if first == second {
		t.Fatal("changing the selection did not change its highlight")
	}
	if ansi.Strip(first) != ansi.Strip(second) {
		t.Fatal("changing selection moved or changed card text")
	}
	view := ansi.Strip(second)
	if strings.Contains(view, "> ") || !strings.Contains(view, "│  1  Quick start") ||
		!strings.Contains(view, "│  2  Advanced configuration") || !strings.Contains(view, "╭─") {
		t.Fatalf("cards lack clean borders and aligned text:\n%s", view)
	}
}

func TestWorkspaceChecklistRequiresExplicitResults(t *testing.T) {
	m := testWorkspace(t)
	m.welcome = false
	m.renderer.SetColorProfile(termenv.TrueColor)
	m.Update(setupChecklistMsg{"Check your environment", []string{"Local tools", "Azure account", "API access"}})
	m.Update(setupCheckMsg{name: "Local tools"})
	m.Update(setupStepMsg("Checking tool versions"))
	m.Update(setupStepMsg("Inspecting kubectl"))
	if m.checks[0].done {
		t.Fatal("progress text marked the operation complete")
	}
	m.Update(setupCheckMsg{name: "Local tools", done: true})
	m.Update(setupCheckMsg{name: "Azure account"})
	m.Update(setupStepMsg(strings.Repeat("Long probe detail ", 20) + "\nmore detail"))
	for _, size := range [][2]int{{110, 30}, {80, 24}, {40, 14}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := ansi.Strip(m.View().Content)
		for _, want := range []string{"Check your environment", "✓ Local tools", "Azure account", "○ API access"} {
			if !strings.Contains(view, want) {
				t.Fatalf("checklist lost stable rows at %dx%d:\n%s", size[0], size[1], view)
			}
		}
		lines := strings.Split(ansi.Strip(m.contextText()), "\n")
		if len(lines) != 4 || !strings.Contains(lines[1], "Azure account") ||
			!strings.HasPrefix(lines[2], "  └─ Long probe detail") || ansi.StringWidth(lines[2]) > m.bodyWidth() ||
			!strings.Contains(lines[3], "○ API access") {
			t.Fatalf("active status was not bounded beneath its owning check: %q", m.contextText())
		}
	}
	failure := errors.New("Azure session expired")
	m.Update(setupCheckMsg{name: "Azure account", done: true, err: failure})
	if strings.Contains(m.contextText(), "Long probe detail") {
		t.Fatal("completed check retained its live status line")
	}
	m.Update(tea.KeyPressMsg{Code: 'l'})
	if !strings.Contains(m.contextText(), strings.Repeat("Long probe detail ", 20)) {
		t.Fatal("activity lost the full progress detail")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m.Update(setupFinishedMsg{failure})
	if !strings.Contains(ansi.Strip(m.contextText()), "✗ Azure account") || m.checks[2].done {
		t.Fatal("failed or pending checks were presented as successful")
	}
	m.Update(setupCheckMsg{name: "Azure account"})
	if m.checks[1].done || m.checks[1].err != nil {
		t.Fatal("retry retained the earlier result")
	}
	m.Update(setupSectionMsg("Configure your installation"))
	if len(m.checks) != 0 {
		t.Fatal("checklist leaked into a question screen")
	}
}

func TestWorkspaceChecklistSpinnerKeepsTextAligned(t *testing.T) {
	m := testWorkspace(t)
	m.welcome = false
	m.Update(setupChecklistMsg{"Validate your deployment", []string{"Local tools", "Azure readiness", "Image builder"}})
	m.Update(setupCheckMsg{name: "Local tools", done: true})
	m.Update(setupCheckMsg{name: "Azure readiness"})
	if strings.Contains(m.checklistView(), "└─") {
		t.Fatal("operation without detail displayed an empty child branch")
	}
	m.Update(setupStepMsg("Loading regional sizes"))
	column := func(line, text string) int {
		index := strings.Index(line, text)
		if index < 0 {
			return -1
		}
		return ansi.StringWidth(line[:index])
	}
	for _, profile := range []termenv.Profile{termenv.TrueColor, termenv.Ascii} {
		m.renderer.SetColorProfile(profile)
		for range m.checkSpinner.Spinner.Frames {
			view := m.checklistView()
			rendered := strings.Split(view, "\n")
			lines := strings.Split(ansi.Strip(view), "\n")
			active := m.renderer.NewStyle().Foreground(lipgloss.Color(setupActive))
			if !strings.HasPrefix(rendered[1], active.Render(m.checkSpinner.View())+" "+active.Bold(true).Render("Azure readiness")) ||
				!strings.HasSuffix(lines[1], "0:00") {
				t.Fatalf("active spinner/status did not use the active blue with a timer: %q", rendered[1])
			}
			if soft := m.renderer.NewStyle().Foreground(lipgloss.Color(setupActiveSoft)); rendered[2] != soft.Render(lines[2]) {
				t.Fatalf("child status did not use the soft active blue: %q", rendered[2])
			}
			if lines[3] != "○ Image builder" {
				t.Fatalf("pending check added redundant status text: %q", lines[3])
			}
			start := column(lines[0], "Local tools")
			if start != column(lines[1], "Azure readiness") || start != column(lines[3], "Image builder") ||
				start+3 != column(lines[2], "Loading regional sizes") || !strings.HasPrefix(lines[2], "  └─ ") {
				t.Fatalf("check rows or child indentation lost their hierarchy:\n%s", strings.Join(lines, "\n"))
			}
			m.checkSpinner, _ = m.checkSpinner.Update(m.checkSpinner.Tick())
		}
	}
}

func TestWorkspacePreparationFollowsItsOwnLog(t *testing.T) {
	m := testWorkspace(t)
	m.session.events = make(chan any, 1024)
	ctx := context.WithValue(t.Context(), setupSessionKey{}, m.session)
	path := testCLIConfig(t)
	stop := errors.New("first stage stopped")
	err := setupForTest(ctx, setupOptions{home: recordHome(path), name: recordName(path), yes: true, skipContextHandoff: true, discovery: subscriptionDisplayRunner(t)},
		strings.NewReader(""), io.Discard, io.Discard, func(context.Context, []string, io.Writer, io.Writer) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("setup did not stop at the first stage: %v", err)
	}
	logPath := ""
	for len(m.session.events) > 0 {
		switch event := (<-m.session.events).(type) {
		case setupLogMsg:
			logPath = cmp.Or(logPath, string(event))
		case setupCheckMsg:
			if logPath == "" {
				t.Fatalf("check %q started before Preparation had a log", event.name)
			}
		}
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	c, err := installer.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	for _, want := range []string{"[step] Local tools", "[step] Azure deployment readiness"} {
		if !strings.Contains(log, want) {
			t.Fatalf("Preparation log is missing %q:\n%s", want, log)
		}
	}
	if first := setupStages(c, false)[0].label; strings.Contains(log, first) {
		t.Fatalf("stage %q wrote to the Preparation log:\n%s", first, log)
	}
}

func TestWorkspaceInstallationPhasesDeclareTheirOperationsBeforeWork(t *testing.T) {
	m := testWorkspace(t)
	m.session.events = make(chan any, 1024)
	ctx := context.WithValue(t.Context(), setupSessionKey{}, m.session)
	drain := func() {
		for {
			select {
			case event := <-m.session.events:
				m.Update(event)
			default:
				return
			}
		}
	}
	expected := map[string]int{"images": 3 + len(installer.ImageCheckSteps()), "cluster": 3, "substrate": 3, "demo": 1, "connect": 1}
	seen := map[string]string{}
	err := setupForTest(ctx, setupOptions{
		home: recordHome(testCLIConfig(t)), name: "substrate-test", yes: true, withDemo: true, skipContextHandoff: true,
		discovery: subscriptionDisplayRunner(t),
	}, strings.NewReader(""), io.Discard, io.Discard,
		func(_ context.Context, args []string, _, _ io.Writer) error {
			drain()
			if len(m.checks) != expected[m.phase] || len(m.checks) == 0 {
				t.Fatalf("phase %s did not declare its operations before execution: %+v", m.phase, m.checks)
			}
			if m.completed[m.phase] {
				t.Fatalf("phase %s completed before all of its operations returned", m.phase)
			}
			var names []string
			active := -1
			for i, check := range m.checks {
				names = append(names, check.name)
				if !check.done && !check.started.IsZero() {
					if active != -1 {
						t.Fatalf("phase %s has multiple active operations", m.phase)
					}
					active = i
				}
			}
			if active == -1 {
				t.Fatalf("phase %s has no active operation", m.phase)
			}
			for i, check := range m.checks {
				if check.done != (i < active) {
					t.Fatalf("phase %s marked unfinished work successful: %+v", m.phase, m.checks)
				}
			}
			labels := strings.Join(names, "\n")
			if previous, ok := seen[m.phase]; ok && previous != labels {
				t.Fatalf("phase %s dynamically changed its operation list", m.phase)
			}
			seen[m.phase] = labels
			if args[0] == "images" && strings.Contains(strings.Join(args, " "), "--check") {
				for _, name := range installer.ImageCheckSteps() {
					m.session.send(setupCheckMsg{name: name})
					m.session.send(setupCheckMsg{name: name, done: true})
				}
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	for phase := range expected {
		if _, ok := seen[phase]; !ok {
			t.Fatalf("did not exercise phase %s", phase)
		}
	}
}

func TestWorkspaceStageChecklistKeepsProgressInlineAndResumesAfterApproval(t *testing.T) {
	m := testWorkspace(t)
	m.welcome = false
	m.Update(setupStepMsg("Last preflight detail"))
	m.Update(setupReviewMsg{{kind: "note", text: "Ready to review"}})
	if strings.Contains(m.contextText(), "[work]") || strings.Contains(m.contextText(), "Last preflight detail") {
		t.Fatal("installation review flashed stale preflight progress")
	}
	items := []string{"Preview registry bootstrap", "Provision the private registry"}
	m.Update(setupChecklistMsg{"Images and assets", items})
	m.Update(setupCheckMsg{name: items[0]})
	for _, detail := range []string{"", "Checking Azure subscription access", "Preparing infrastructure templates"} {
		if detail != "" {
			m.Update(setupStepMsg(detail))
		}
		m.Update(setupNoteMsg{text: "Target: redundant / eastus2"})
		text := ansi.Strip(m.contextText())
		lines := strings.Split(text, "\n")
		// A check without step detail leaves no blank row under it.
		want := 3
		if detail == "" {
			want = 2
		}
		if len(lines) != want || !strings.Contains(lines[0], items[0]) ||
			!strings.Contains(lines[want-1], "○ "+items[1]) || strings.Contains(text, "[work]") ||
			strings.Contains(text, "Target:") || m.checks[0].done {
			t.Fatalf("progress changed the stable checklist layout or completion state:\n%s", text)
		}
	}
	m.Update(setupPlanMsg{"test", []installer.ResourceChange{{ChangeType: "Create", ResourceID: "/resourceGroups/test"}}})
	m.Update(setupCheckMsg{name: items[0], done: true, err: errors.New("Preview failed after producing output")})
	if m.plan || !strings.Contains(m.contextText(), "Preview failed after producing output") {
		t.Fatal("partial preview hid the failed operation's retry explanation")
	}
	m.Update(setupCheckMsg{name: items[0]})
	m.Update(setupPlanMsg{"test", []installer.ResourceChange{{ChangeType: "Create", ResourceID: "/resourceGroups/test"}}})
	m.Update(setupCheckMsg{name: items[0], done: true})
	if !strings.Contains(ansi.Strip(m.contextText()), "to create") {
		t.Fatal("checklist hid the infrastructure approval")
	}
	m.Update(setupCheckMsg{name: items[1]})
	text := ansi.Strip(m.contextText())
	if m.plan || m.title != "Images and assets" || !strings.Contains(text, "✓ "+items[0]) ||
		!strings.Contains(text, items[1]) || strings.Contains(text, "to create") ||
		strings.Contains(text, "Preparing infrastructure templates") {
		t.Fatalf("approval did not return to the preserved checklist:\n%s", text)
	}
	m.Update(setupCheckMsg{name: items[1], done: true, err: errors.New("Azure request failed")})
	if text := ansi.Strip(m.contextText()); !strings.Contains(text, "✗ "+items[1]) || !strings.Contains(text, "Azure request failed") {
		t.Fatalf("failed operation lost its result or retry explanation:\n%s", text)
	}
	m.Update(setupCheckMsg{name: items[1]})
	if text := ansi.Strip(m.contextText()); strings.Contains(text, "Azure request failed") || strings.Contains(text, "Failed") {
		t.Fatalf("retry retained its stale failure:\n%s", text)
	}
	m.Update(setupNoteMsg{text: "Explicit warning remains visible", warning: true})
	if !strings.Contains(m.contextText(), "WARNING: Explicit warning remains visible") {
		t.Fatal("checklist hid a warning with the normal progress chatter")
	}
}

func TestWorkspaceImageVerificationChecklistRetriesAllSteps(t *testing.T) {
	for _, failure := range []string{"before command starts", "source fetch"} {
		t.Run(failure, func(t *testing.T) {
			m := testWorkspace(t)
			m.welcome = false
			items := installer.ImageCheckSteps()
			m.Update(setupChecklistMsg{"Images and assets", items})
			m.Update(setupCheckGroupMsg{names: items})
			m.Update(setupCheckMsg{name: items[0]})
			want := errors.New("verification failed")
			failed := 0
			if failure == "source fetch" {
				failed = 1
				m.Update(setupCheckMsg{name: items[0], done: true})
				m.Update(setupCheckMsg{name: items[1]})
				m.Update(setupStepMsg("Fetching Substrate source at the configured commit"))
				text := ansi.Strip(m.contextText())
				for _, pending := range items[2:] {
					if !strings.Contains(text, "○ "+pending) {
						t.Fatalf("source fetch hid a future verification step:\n%s", text)
					}
				}
				m.Update(setupCheckMsg{name: items[1], done: true, err: want})
			}
			m.Update(setupCheckGroupMsg{names: items, done: true, err: want})
			for i, check := range m.checks {
				if check.done != (i <= failed) || (check.err != nil) != (i == failed) {
					t.Fatalf("failure marked the wrong image operation: %+v", m.checks)
				}
			}
			m.Update(setupCheckGroupMsg{names: items})
			m.Update(setupCheckMsg{name: items[0]})
			// A command-level success cannot substitute for individual step results.
			m.Update(setupCheckGroupMsg{names: items, done: true})
			for i, check := range m.checks {
				if check.done || check.err != nil || check.started.IsZero() != (i > 0) {
					t.Fatalf("retry retained a stale result or completed unreported work: %+v", m.checks)
				}
			}
		})
	}
}

func TestWorkspaceCombinedImageChecklistKeepsActiveVerificationVisible(t *testing.T) {
	for _, width := range []int{96, 40} {
		m := testWorkspace(t)
		m.welcome = false
		m.Update(tea.WindowSizeMsg{Width: width, Height: 14})
		m.Update(setupPhaseMsg("images"))
		var items []string
		for _, stage := range setupStages(installer.Config{}, false) {
			if setupPhase(stage.command, stage.flags) == "images" {
				items = append(items, setupStageChecks(stage.label, stage.command, stage.flags)...)
			}
		}
		m.Update(setupChecklistMsg{"Images and assets", items})
		for i, name := range items {
			m.Update(setupCheckMsg{name: name})
			if i < len(items)-1 {
				m.Update(setupCheckMsg{name: name, done: true})
			}
		}
		m.Update(setupStepMsg("Checking saved image inventory"))
		view := strings.Join(strings.Fields(ansi.Strip(m.View().Content)), " ")
		if !strings.Contains(view, items[len(items)-1]) || !strings.Contains(view, "Checking saved image inventory") {
			t.Fatalf("width %d hid the active operation in the merged checklist:\n%s", width, m.View().Content)
		}
		workspaceKey(m, 'l')
		workspaceKey(m, tea.KeyEsc)
		if !strings.Contains(ansi.Strip(m.View().Content), "Checking saved image inventory") {
			t.Fatal("closing logs lost the current operation in the merged checklist")
		}
	}
}

func TestWorkspaceCombinedImageChecklistShowsFailedRegistryStepDuringRetry(t *testing.T) {
	m := testWorkspace(t)
	m.welcome = false
	m.renderer.SetColorProfile(termenv.TrueColor)
	m.Update(tea.WindowSizeMsg{Width: 96, Height: 20})
	m.Update(setupPhaseMsg("images"))
	var items []string
	for _, stage := range setupStages(installer.Config{}, false) {
		if setupPhase(stage.command, stage.flags) == "images" {
			items = append(items, setupStageChecks(stage.label, stage.command, stage.flags)...)
		}
	}
	m.Update(setupChecklistMsg{"Images and assets", items})
	m.Update(setupCheckMsg{name: items[0], done: true})
	m.Update(setupCheckMsg{name: items[1]})
	m.Update(setupCheckMsg{name: items[1], done: true, err: errors.New("Registry access denied")})
	m.Update(setupFormMsg{form: huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("Retry this stage?"))), done: make(chan error, 1)})
	view := ansi.Strip(m.View().Content)
	for _, want := range []string{"Provision the private registry", "Registry access denied", "Retry this stage?"} {
		if !strings.Contains(view, want) {
			t.Fatalf("merged checklist hid retry context %q:\n%s", want, view)
		}
	}
	if !strings.Contains(m.checklistView(), m.renderer.NewStyle().Foreground(lipgloss.Color("1")).Render("  └─ Registry access denied")) {
		t.Fatal("failed child status lost its branch or error color")
	}
}

func TestWorkspaceStageChecklistDoesNotAutoScrollDestructiveApproval(t *testing.T) {
	m := testWorkspace(t)
	m.welcome = false
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.Update(setupChecklistMsg{"AKS cluster", []string{"Preview full infrastructure", "Provision AKS"}})
	m.Update(setupCheckMsg{name: "Preview full infrastructure"})
	var changes []installer.ResourceChange
	for range 30 {
		changes = append(changes, installer.ResourceChange{ChangeType: "Delete", ResourceID: "/resourceGroups/test/providers/Microsoft.Storage/storageAccounts/old"})
	}
	m.Update(setupPlanMsg{"test", changes})
	m.Update(setupCheckMsg{name: "Preview full infrastructure", done: true})
	value := false
	m.Update(setupFormMsg{form: huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Apply these planned changes?").Value(&value))), done: make(chan error, 1)})
	if m.scroll != 0 || !m.reviewRequired || m.contextViewport().AtBottom() {
		t.Fatal("preserved checklist scrolled a destructive review directly to the bottom")
	}
	workspaceKey(m, 'a')
	if m.reviewed {
		t.Fatal("destructive approval bypassed the full-summary review")
	}
}

func TestSetupChecklistReportsOperationErrors(t *testing.T) {
	m := testWorkspace(t)
	ui := newSetupUI(io.Discard)
	ui.session = m.session
	want := errors.New("probe failed")
	ui.checklist("Checks", "Probe")
	if err := ui.check("Probe", func() error {
		ui.step("Probe emitted progress")
		return want
	}); !errors.Is(err, want) {
		t.Fatalf("check changed the operation error: %v", err)
	}
	for len(m.session.events) > 0 {
		m.Update(<-m.session.events)
	}
	if len(m.checks) != 1 || !m.checks[0].done || !errors.Is(m.checks[0].err, want) {
		t.Fatal("operation failure was not explicitly reported")
	}
}

func TestWorkspaceReviewGroupsRowsUnderHeadings(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	c := installer.DefaultConfig()
	c.AKS.ClusterName = "review-cluster"
	c.AKS.AllowUnrestrictedAPI = true
	c.Azure.SubscriptionID = "22222222-2222-4222-8222-222222222222"
	c.Azure.AssetPublisherObjectID = "33333333-3333-4333-8333-333333333333"
	c.AKS.Operator = &installer.OperatorConfig{ObjectID: c.Azure.AssetPublisherObjectID, PrincipalType: "User"}
	for _, width := range []int{140, 36} {
		m := testWorkspace(t)
		m.welcome = false
		m.Update(tea.WindowSizeMsg{Width: width, Height: 60})
		ui := newSetupUI(io.Discard)
		ui.session = m.session
		ui.summary(c, azureDefaults{subscriptionName: "Dev subscription", publisherName: "Pat Publisher"}, "/home/tester/.local/state/substrate-aks/installations/x/installation.json", true, "docker")
		m.Update(<-m.session.events)
		lines := strings.Split(ansi.Strip(m.body), "\n")
		find := func(text string) int {
			for i, line := range lines {
				if strings.Contains(line, text) {
					return i
				}
			}
			t.Fatalf("width %d: review lost %q:\n%s", width, text, strings.Join(lines, "\n"))
			return -1
		}
		if find("Dev subscription")+1 != find(c.Azure.SubscriptionID[:13]) || strings.Contains(m.body, "Dev subscription (") {
			t.Fatalf("width %d: subscription ID is not on its own line under the name", width)
		}
		if azure, cluster, access := find("Azure ─"), find("Cluster ─"), find("Access ─"); !(azure < find("Resource group") && find("Resource group") < cluster && cluster < find("API server access") && find("API server access") < access && access < find("Snapshot inspection")) {
			t.Fatalf("width %d: rows are not under their headings", width)
		}
		// The operator is the Azure identity, so it is a role, not a second row.
		if strings.Contains(m.body, "Cluster admin ") || !strings.Contains(strings.Join(strings.Fields(ansi.Strip(m.body)), " "), "cluster admin") ||
			strings.Count(m.body, c.Azure.AssetPublisherObjectID[:13]) != 1 {
			t.Fatalf("width %d: the Azure identity is repeated instead of listing its roles", width)
		}
		if find("Snapshot inspection") >= find("Existing operator") || find("Before you continue") >= find("⚠ "+unrestrictedAPIWarning[:20]) {
			t.Fatalf("width %d: explanation or warning moved away from its row or box", width)
		}
		if width > 60 {
			find("~/.local/state/substrate-aks/installations/x/installation.json")
		}
		if boxed := strings.Contains(m.body, "╭"); boxed != (width > 60) {
			t.Fatalf("width %d: callout boxed=%v", width, boxed)
		}
	}
}

func TestWorkspaceArrowsScrollReviewWhileConfirmIsFocused(t *testing.T) {
	m := testWorkspace(t)
	m.welcome = false
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	m.Update(setupReviewMsg{{kind: "note", text: strings.Repeat("review line\n", 40)}})
	value := false
	m.Update(setupFormMsg{form: huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Continue?").Value(&value))), done: make(chan error, 1)})
	if !strings.Contains(ansi.Strip(m.View().Content), "↑↓ to scroll") {
		t.Fatalf("scroll hint does not name usable keys:\n%s", ansi.Strip(m.View().Content))
	}
	workspaceKey(m, tea.KeyDown)
	workspaceKey(m, 'j')
	if m.scroll != 2 || value {
		t.Fatalf("arrows did not scroll the review (scroll=%d, value=%v)", m.scroll, value)
	}
	workspaceKey(m, tea.KeyUp)
	if m.scroll != 1 {
		t.Fatalf("up did not scroll back: %d", m.scroll)
	}
}

func TestWorkspaceReviewAndPromptColorsRespectOutputProfile(t *testing.T) {
	for _, profile := range []termenv.Profile{termenv.TrueColor, termenv.Ascii} {
		m := testWorkspace(t)
		m.welcome = false
		m.renderer.SetColorProfile(profile)
		ui := newSetupUI(io.Discard)
		ui.session = m.session
		c := installer.DefaultConfig()
		c.AKS.ClusterName = "review-cluster\x1b]2;untrusted\x07"
		ui.summary(c, azureDefaults{}, "deploy.json", true, "docker")
		m.Update(<-m.session.events)
		if !strings.Contains(ansi.Strip(m.body), "review-cluster") ||
			!strings.Contains(ansi.Strip(m.body), "⚠ This creates billable") || strings.Contains(m.body, "\x1b]") {
			t.Fatalf("review lost content or allowed terminal control: %q", m.body)
		}
		value := false
		m.Update(setupFormMsg{form: huh.NewForm(huh.NewGroup(
			huh.NewConfirm().Title("Continue with this billable installation?").Value(&value))), done: make(chan error, 1)})
		coloredReview := strings.Contains(m.body, "\x1b[")
		beforeTitle, _, found := strings.Cut(m.form.View(), "Continue with")
		if !found {
			t.Fatal("prompt title disappeared")
		}
		coloredPrompt := strings.Contains(beforeTitle, "\x1b[38;")
		if profile == termenv.TrueColor && !strings.Contains(beforeTitle, "180;140;242") {
			t.Fatalf("embedded prompt did not use the primary purple: %q", beforeTitle)
		}
		if coloredReview != (profile == termenv.TrueColor) || coloredPrompt != (profile == termenv.TrueColor) {
			t.Fatalf("profile %v: review colors=%v, prompt colors=%v\n%s", profile, coloredReview, coloredPrompt, m.form.View())
		}
	}
}

func TestWorkspaceKeepsTargetAndFormVisibleAtConfirmation(t *testing.T) {
	for _, size := range [][2]int{{120, 32}, {96, 24}, {80, 24}, {48, 18}, {80, 14}, {40, 14}} {
		m := testWorkspace(t)
		m.welcome = false
		c := installer.DefaultConfig()
		c.AKS.ClusterName, c.Azure.Location, c.Azure.SubscriptionID = "test-install", "eastus2", "subscription"
		m.Update(setupTargetMsg{c, true})
		m.Update(setupPhaseMsg("images"))
		m.Update(setupPlanMsg{"subscription", []installer.ResourceChange{{ChangeType: "Create", ResourceID: "/subscriptions/subscription/resourceGroups/test-install"}}})
		value := false
		m.Update(setupFormMsg{form: huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("Continue with this billable installation?").Value(&value))), done: make(chan error, 1)})
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := ansi.Strip(m.View().Content)
		for _, want := range []string{"test-install", "Continue with this", "Yes", "No", "ctrl+c"} {
			if !strings.Contains(view, want) {
				t.Fatalf("%dx%d missing %q:\n%s", size[0], size[1], want, view)
			}
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > size[0] {
				t.Fatalf("line exceeded terminal width: %q", line)
			}
		}
		if lines := strings.Count(view, "\n") + 1; lines > size[1] {
			t.Fatalf("screen exceeded height: %d", lines)
		}
	}
}

func TestWorkspaceInstallationFormKeepsAllFieldsVisible(t *testing.T) {
	m := testWorkspace(t)
	m.welcome = false
	m.Update(setupSectionMsg("Configure your installation"))
	prompt := &setupPrompt{ui: &setupUI{session: m.session}}
	regions := []string{
		"australiaeast", "australiasoutheast", "brazilsouth", "canadacentral",
		"centralindia", "centralus", "eastasia", "eastus", "eastus2",
		"francecentral", "germanywestcentral", "japaneast", "koreacentral",
		"northeurope", "norwayeast", "southafricanorth", "southcentralus",
		"southeastasia", "swedencentral", "switzerlandnorth", "uksouth",
		"westcentralus", "westeurope", "westus", "westus2", "westus3",
	}
	finished := make(chan struct{})
	t.Cleanup(func() {
		m.cancel()
		<-finished
	})
	go func() {
		defer close(finished)
		_, _, _ = prompt.installation(m.session.ctx, "prefilled-cluster", "eastus2", regions, "203.0.113.8/32", new(bool), new(bool))
	}()
	select {
	case event := <-m.session.events:
		m.Update(event)
	case <-time.After(time.Second):
		t.Fatal("installation form was not presented")
	}
	m.Update(tea.PasteMsg{Content: "Bad_Name"})
	if view := ansi.Strip(m.View().Content); !strings.Contains(view, "Invalid name:") {
		t.Fatalf("embedded name input lost live validation:\n%s", view)
	}
	m.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	m.Update(tea.PasteMsg{Content: "prefilled-cluster"})
	for _, size := range [][2]int{{120, 32}, {80, 24}, {48, 18}, {40, 14}, {120, 32}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := ansi.Strip(m.View().Content)
		want := []string{"Dedicated resource group", "prefilled-cluster", "ctrl+c"}
		want = append(want, "Azure region")
		if size[1] >= 18 {
			want = append(want, "eastus2")
		}
		if size[1] >= 24 {
			want = append(want, "AKS API server access", "Only my IP address (203.0.113.8/32)", "Anyone on the internet",
				"Grant this Azure identity", "Snapshots can contain")
		}
		for _, text := range want {
			if !strings.Contains(view, text) {
				t.Fatalf("%dx%d missing %q:\n%s", size[0], size[1], text, view)
			}
		}
		titleLine, nameLine, visibleRegions := -1, -1, 0
		for i, line := range strings.Split(view, "\n") {
			if strings.Contains(line, "Configure your installation") {
				titleLine = i
			}
			if strings.Contains(line, "Dedicated resource group") {
				nameLine = i
			}
			fields := strings.Fields(line)
			if len(fields) > 0 && slices.Contains(regions, fields[len(fields)-1]) {
				visibleRegions++
			}
			if ansi.StringWidth(line) > size[0] {
				t.Fatalf("%dx%d exceeded terminal width:\n%s", size[0], size[1], view)
			}
		}
		if lipgloss.Height(view) > size[1] {
			t.Fatalf("%dx%d exceeded terminal height:\n%s", size[0], size[1], view)
		}
		gap := 2
		if size[1] <= 18 {
			gap = 1
		}
		if nameLine-titleLine != gap {
			t.Fatalf("%dx%d reserved empty context above the form:\n%s", size[0], size[1], view)
		}
		// The API and snapshot questions below the list take ten rows.
		if size[1] >= 24 && visibleRegions < size[1]-24 {
			t.Fatalf("%dx%d left the region list too short (%d visible):\n%s", size[0], size[1], visibleRegions, view)
		}
	}
}

func TestWorkspaceShortContextKeepsPromptNearTop(t *testing.T) {
	for _, size := range [][2]int{{120, 48}, {80, 40}, {40, 14}} {
		m := testWorkspace(t)
		m.welcome = false
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m.Update(setupTargetMsg{installer.DefaultConfig(), false})
		m.Update(setupChecklistMsg{"Validate your deployment", []string{"Runtime tools", "Image builder"}})
		m.Update(setupCheckMsg{name: "Runtime tools", done: true})
		m.Update(setupCheckMsg{name: "Image builder"})
		m.Update(setupNoteMsg{text: "Both image builders are available."})
		engine := "docker"
		m.Update(setupFormMsg{form: huh.NewForm(huh.NewGroup(
			huh.NewSelect[string]().Title("Choose an image builder").Options(
				huh.NewOption("Docker", "docker"), huh.NewOption("Podman", "podman")).Value(&engine))), done: make(chan error, 1)})
		view := ansi.Strip(m.View().Content)
		for _, want := range []string{"? Image builder", "Docker", "Podman"} {
			if !strings.Contains(view, want) {
				t.Fatalf("image-builder choice lost its checklist context or options %q:\n%s", want, view)
			}
		}
		if size[1] >= 24 && !strings.Contains(view, "✓ Runtime tools") {
			t.Fatalf("image-builder question hid completed checks:\n%s", view)
		}
		promptLine := -1
		for i, line := range strings.Split(view, "\n") {
			if strings.Contains(line, "Choose an image builder") {
				promptLine = i
			}
		}
		if promptLine < 0 || promptLine > 12 {
			t.Fatalf("%dx%d placed a short question far from its context (line %d):\n%s", size[0], size[1], promptLine, view)
		}
	}
}

func TestWorkspaceFrameStaysStableAcrossPreparationAndApprovals(t *testing.T) {
	for _, size := range [][2]int{{145, 32}, {110, 24}, {96, 14}, {80, 24}, {48, 18}, {40, 14}} {
		m := testWorkspace(t)
		m.welcome = false
		m.renderer.SetColorProfile(termenv.Ascii)
		m.phases = setupPhases(installer.Config{}, true)
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		initialHeader := strings.Split(ansi.Strip(m.headerView()), "\n")
		initialWidth := m.bodyWidth()
		if len(initialHeader) != 1 || (size[0] >= 80 &&
			!strings.Contains(initialHeader[0], "cluster — · region — · subscription —")) {
			t.Fatalf("fresh installation did not reserve all target fields: %q", initialHeader)
		}
		c := installer.DefaultConfig()
		c.AKS.ClusterName, c.Azure.Location, c.Azure.SubscriptionID = "fresh-cluster", "eastus2", defaultSubscription
		form := func(title string) setupFormMsg {
			return setupFormMsg{form: huh.NewForm(huh.NewGroup(huh.NewConfirm().Title(title))), done: make(chan error, 1)}
		}
		for _, event := range []tea.Msg{
			setupChecklistMsg{"Check your environment", []string{"Local tools", "Azure account"}},
			setupCheckMsg{name: "Local tools"},
			setupStepMsg("Checking Azure CLI"),
			setupSectionMsg("Configure your installation"),
			setupFormMsg{form: huh.NewForm(huh.NewGroup(huh.NewInput().Title("Cluster name"))), done: make(chan error, 1)},
			setupTargetMsg{c, true},
			setupReviewMsg{{kind: "note", text: "Review these installation settings."}},
			form("Continue with this billable installation?"),
			setupPlanMsg{defaultSubscription, []installer.ResourceChange{{ChangeType: "Create", ResourceID: "/resourceGroups/fresh-cluster"}}},
			form("Apply these planned changes?"),
			tea.KeyPressMsg{Code: 'd', Text: "d"},
			tea.KeyPressMsg{Code: tea.KeyEsc},
			tea.KeyPressMsg{Code: 'l', Text: "l"},
			tea.KeyPressMsg{Code: '?', Text: "?"},
			tea.KeyPressMsg{Code: tea.KeyEsc},
			setupChecklistMsg{"Validate your deployment", []string{"Azure readiness"}},
			setupCheckMsg{name: "Azure readiness"},
			setupCheckMsg{name: "Azure readiness", done: true, err: errors.New("verification failed")},
			form("Retry this stage?"),
		} {
			m.Update(event)
			header := strings.Split(ansi.Strip(m.headerView()), "\n")
			if len(header) != len(initialHeader) || m.bodyWidth() != initialWidth {
				t.Fatalf("%dx%d: %T changed the frame geometry", size[0], size[1], event)
			}
			view := ansi.Strip(m.View().Content)
			lines := strings.Split(view, "\n")
			if len(lines) != size[1] || !strings.Contains(lines[len(header)], "──") ||
				!strings.Contains(lines[size[1]-2], "──") {
				t.Fatalf("%dx%d: %T displaced a frame separator:\n%s", size[0], size[1], event, view)
			}
			for _, line := range lines {
				if ansi.StringWidth(line) > size[0] {
					t.Fatalf("%T overflowed the frame width:\n%s", event, view)
				}
			}
			for _, phase := range m.phases {
				mark := "○"
				if phase == "prepare" {
					mark = "●"
				}
				present := strings.Contains(view, mark+" "+setupPhaseTitle(phase))
				if present != (size[0] >= 96) {
					t.Fatalf("%dx%d: %T changed rail visibility for %s:\n%s", size[0], size[1], event, phase, view)
				}
			}
		}
	}
}

func TestWorkspaceDetailsShowTheReviewOnce(t *testing.T) {
	m := testWorkspace(t)
	m.welcome = false
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.Update(setupReviewMsg{{kind: "row", label: "Resource group", text: "review-marker"}})
	m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	if m.view != "details" || strings.Count(ansi.Strip(m.View().Content), "review-marker") != 1 {
		t.Fatalf("details repeated the installation review:\n%s", ansi.Strip(m.View().Content))
	}
}

func TestWorkspaceRailProgressHatchesTheRunningStage(t *testing.T) {
	m := testWorkspace(t)
	m.welcome = false
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	bar := func() string { return strings.TrimSpace(strings.Split(ansi.Strip(m.rail()), "\n")[1]) }
	barWidth, total := setupRailWidth-2, len(m.phases)
	for i, phase := range m.phases {
		m.Update(setupPhaseMsg(phase))
		done, hatched := barWidth*i/total, barWidth*(i+1)/total-barWidth*i/total
		if want := strings.Repeat("━", done) + strings.Repeat("╍", hatched) + strings.Repeat("─", barWidth-done-hatched); bar() != want {
			t.Fatalf("stage %d of %d bar = %q, want finished stages solid and the running stage hatched: %q", i+1, total, bar(), want)
		}
	}
	m.Update(setupFinishedMsg{})
	if want := strings.Repeat("━", barWidth); bar() != want {
		t.Fatalf("completed installation bar = %q, want %q", bar(), want)
	}
}

func TestWorkspaceRailReservesMarkersForEveryPhaseState(t *testing.T) {
	m := testWorkspace(t)
	m.welcome = false
	m.renderer.SetColorProfile(termenv.TrueColor)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	initial := ansi.Strip(m.rail())
	if !strings.HasPrefix(initial, "STAGE 1 OF 5") || !strings.Contains(initial, "● Preparation") ||
		!strings.Contains(initial, "○ Images and assets") ||
		strings.Contains(initial, "Installation") || strings.Contains(initial, "Private registry") {
		t.Fatalf("fresh installation lacks active/pending phase markers:\n%s", initial)
	}
	for _, phase := range m.phases {
		m.Update(setupPhaseMsg(phase))
		purple := m.renderer.NewStyle().Foreground(lipgloss.Color(setupAccent))
		active := purple.Render("●") + " " + purple.Bold(true).Render(setupPhaseTitle(phase))
		if !strings.Contains(m.rail(), active) {
			t.Fatalf("active phase %s did not use the primary purple marker", phase)
		}
		for _, line := range strings.Split(ansi.Strip(m.rail()), "\n") {
			for _, titlePhase := range m.phases {
				title := setupPhaseTitle(titlePhase)
				if i := strings.Index(line, title); i >= 0 && ansi.StringWidth(line[:i]) != 2 {
					t.Fatalf("phase labels moved when their state changed: %q", line)
				}
			}
		}
	}
	m.Update(setupFinishedMsg{})
	rail := ansi.Strip(m.rail())
	for _, phase := range m.phases {
		if !strings.Contains(rail, "✓ "+setupPhaseTitle(phase)) {
			t.Fatalf("successful installation left an incorrect phase marker:\n%s", rail)
		}
	}
	m = testWorkspace(t)
	m.Update(setupPhaseMsg("cluster"))
	m.Update(setupFinishedMsg{errors.New("provisioning failed")})
	rail = ansi.Strip(m.rail())
	if !strings.Contains(rail, "✓ Preparation") || !strings.Contains(rail, "✗ AKS cluster") ||
		!strings.Contains(rail, "○ Substrate") {
		t.Fatalf("failure marked completed or pending phases incorrectly:\n%s", rail)
	}
}

func TestWorkspaceHeaderStacksTargetBesideBrand(t *testing.T) {
	for _, width := range []int{128, 96, 80, 40} {
		m := testWorkspace(t)
		m.welcome = false
		m.Update(tea.WindowSizeMsg{Width: width, Height: 32})
		c := installer.DefaultConfig()
		c.AKS.ClusterName, c.Azure.Location = "substrate-dev-tui", "eastus2"
		c.Azure.SubscriptionID = "11111111-1111-4111-8111-111111111111"
		m.Update(setupTargetMsg{c, false})
		m.Update(setupFormMsg{form: huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("Continue?"))), done: make(chan error, 1)})
		header := ansi.Strip(m.headerView())
		if !strings.HasPrefix(header, "◆ Agent Substrate on AKS") || strings.Contains(header, "\n") ||
			strings.Contains(header, "Elapsed") || strings.Contains(header, "DECISION") || strings.Contains(header, "WORKING") {
			t.Fatalf("header retained status noise, lost branding or left one line:\n%s", header)
		}
		if width >= 80 && (!strings.Contains(header, c.AKS.ClusterName) || !strings.Contains(header, "eastus2") ||
			!strings.HasSuffix(header, "11111111…") && !strings.HasSuffix(header, c.Azure.SubscriptionID)) {
			t.Fatalf("target metadata is not right-aligned beside the brand:\n%s", header)
		}
		if width >= 128 && !strings.HasSuffix(header, "subscription "+c.Azure.SubscriptionID) {
			t.Fatalf("wide header abbreviated a subscription that fits:\n%s", header)
		}
		for _, line := range strings.Split(ansi.Strip(m.View().Content), "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatalf("header or body overflowed width %d:\n%s", width, line)
			}
		}
		if width >= 80 {
			m.cluster = strings.Repeat("x", 63)
			header = ansi.Strip(m.headerView())
			if !strings.Contains(header, "eastus2") || !strings.HasSuffix(header, "11111111…") {
				t.Fatalf("long cluster name displaced the region or subscription:\n%s", header)
			}
			for _, line := range strings.Split(header, "\n") {
				if ansi.StringWidth(line) > width {
					t.Fatalf("long cluster name overflowed width %d:\n%s", width, line)
				}
			}
		}
	}
}

func TestWorkspacePlanApprovalPresentation(t *testing.T) {
	prefix := "/subscriptions/test/resourceGroups/example"
	for _, size := range [][2]int{{120, 32}, {96, 24}, {40, 14}} {
		m := testWorkspace(t)
		m.welcome = false
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m.Update(setupTargetMsg{installer.DefaultConfig(), false})
		m.Update(setupPhaseMsg("images"))
		m.Update(setupPlanMsg{"test", []installer.ResourceChange{
			{ChangeType: "Create", ResourceID: prefix},
			{ChangeType: "Create", ResourceID: prefix + "/providers/Microsoft.ContainerRegistry/registries/example"},
			{ChangeType: "Create", ResourceID: prefix + "/providers/Microsoft.Authorization/roleAssignments/example"},
		}})
		m.Update(setupNoteMsg{text: "Infrastructure preview complete (22s)."})
		if m.title != "Review registry changes" {
			t.Fatalf("merging the image phase lost the registry-specific approval title: %s", m.title)
		}
		accepted := false
		m.Update(setupFormMsg{form: huh.NewForm(huh.NewGroup(
			huh.NewConfirm().Title("Apply these planned changes?").Value(&accepted))), done: make(chan error, 1)})
		body := ansi.Strip(m.body)
		for _, want := range []string{"3 to create", "Resource group", "Container registry", "Role assignments"} {
			if !strings.Contains(body, want) {
				t.Fatalf("plan lost its meaningful summary %q:\n%s", want, body)
			}
		}
		for _, clutter := range []string{"Scope:", "Microsoft.", "0 modify", "preview complete"} {
			if strings.Contains(body, clutter) {
				t.Fatalf("approval includes redundant diagnostics %q:\n%s", clutter, body)
			}
		}
		if !strings.Contains(strings.Join(m.activity, "\n"), "preview complete") {
			t.Fatal("presentation change discarded the diagnostic activity")
		}
		view := ansi.Strip(m.View().Content)
		for _, want := range []string{"Review registry changes", "Apply these planned", "Yes", "No", "d details"} {
			if !strings.Contains(view, want) {
				t.Fatalf("%dx%d hid the approval %q:\n%s", size[0], size[1], want, view)
			}
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > size[0] {
				t.Fatalf("approval exceeded terminal width:\n%s", view)
			}
		}
		m.Update(setupPlanMsg{"test", []installer.ResourceChange{{ChangeType: "NoChange", ResourceID: prefix}}})
		if !strings.Contains(m.body, "No resource changes") || m.reviewRequired {
			t.Fatal("unchanged infrastructure was presented as new or destructive work")
		}
	}
}

func TestWorkspacePlanLabelsActionsWithinEachResourceCategory(t *testing.T) {
	prefix := "/subscriptions/test/resourceGroups/example/providers/"
	changes := []installer.ResourceChange{
		{ChangeType: "Create", ResourceID: prefix + "Microsoft.ContainerService/managedClusters/aks"},
		{ChangeType: "Modify", ResourceID: prefix + "Microsoft.ContainerRegistry/registries/acr"},
		{ChangeType: "Create", ResourceID: prefix + "Microsoft.Storage/storageAccounts/one"},
		{ChangeType: "Create", ResourceID: prefix + "Microsoft.Storage/storageAccounts/two"},
		{ChangeType: "Modify", ResourceID: prefix + "Microsoft.Storage/storageAccounts/three"},
	}
	for _, width := range []int{120, 96, 40} {
		for _, profile := range []termenv.Profile{termenv.TrueColor, termenv.Ascii} {
			m := testWorkspace(t)
			m.Update(tea.WindowSizeMsg{Width: width, Height: 32})
			m.renderer.SetColorProfile(profile)
			body := m.planView(changes, false)
			text := strings.Join(strings.Fields(ansi.Strip(body)), " ")
			for _, want := range []string{"AKS cluster 1 create", "Container registry 1 update", "Storage account 2 create 1 update"} {
				if !strings.Contains(text, want) {
					t.Fatalf("width %d lost resource-specific action counts %q:\n%s", width, want, body)
				}
			}
			for _, action := range []struct{ color, label string }{{"2", "1 create"}, {"3", "1 update"}} {
				expected := m.renderer.NewStyle().Foreground(lipgloss.Color(action.color)).Render(action.label)
				if !strings.Contains(body, expected) {
					t.Fatalf("action %q lost its color coding:\n%s", action.label, body)
				}
			}
			for _, line := range strings.Split(body, "\n") {
				if ansi.StringWidth(line) > m.bodyWidth() {
					t.Fatalf("action labels overflowed width %d:\n%s", width, body)
				}
			}
		}
	}
}

func TestWorkspaceLetterShortcutsPreserveFormsAndTypedInput(t *testing.T) {
	m := testWorkspace(t)
	m.welcome = false
	value := false
	reply := make(chan error, 1)
	m.Update(setupFormMsg{form: huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("Apply?").Value(&value))), done: reply})
	for _, shortcut := range []struct {
		key  rune
		view string
	}{{'d', "details"}, {'l', "activity"}, {'?', "help"}} {
		workspaceKey(m, shortcut.key)
		if m.view != shortcut.view {
			t.Fatalf("%c did not open %s", shortcut.key, shortcut.view)
		}
		workspaceKey(m, 'y')
		workspaceKey(m, tea.KeyEnter)
		if value || len(reply) != 0 || m.form == nil {
			t.Fatal("overlay keys answered the hidden approval")
		}
		workspaceKey(m, tea.KeyEsc)
	}
	var text string
	m.Update(setupFormMsg{form: huh.NewForm(huh.NewGroup(huh.NewInput().Title("Name").Value(&text))), done: make(chan error, 1)})
	for _, key := range "dla?" {
		m.Update(tea.KeyPressMsg{Code: key, Text: string(key)})
	}
	if text != "dla?" || m.view != "" {
		t.Fatalf("shortcuts stole typed characters: text=%q view=%q", text, m.view)
	}
	selection := huh.NewSelect[string]().Title("Region").
		Options(huh.NewOption("dallas", "dallas"), huh.NewOption("london", "london"))
	m.Update(setupFormMsg{form: huh.NewForm(huh.NewGroup(selection)), done: make(chan error, 1)})
	for _, key := range "/lo" {
		m.Update(tea.KeyPressMsg{Code: key, Text: string(key)})
	}
	if m.view != "" || !selection.GetFiltering() {
		t.Fatal("logs shortcut stole input from a choice filter")
	}
	if hovered, ok := selection.Hovered(); !ok || hovered != "london" {
		t.Fatal("choice filter did not receive its typed shortcut letter")
	}
}

func TestWorkspaceProgressDetailsAndPhaseTruth(t *testing.T) {
	m := testWorkspace(t)
	m.welcome = false
	c := installer.DefaultConfig()
	c.Substrate.Images = installer.ImagesConfig{Repository: "configured"}
	m.Update(setupTargetMsg{c, false})
	if strings.Contains(strings.Join(m.phases, " "), "registry") || strings.Contains(strings.Join(m.phases, " "), "demo") {
		t.Fatalf("invented phases for image reuse/omitted demo: %v", m.phases)
	}
	m.Update(setupPhaseMsg("images"))
	m.Update(setupStepMsg("Verify saved images"))
	m.Update(setupPhaseMsg("cluster"))
	if !m.completed["images"] || m.completed["cluster"] {
		t.Fatalf("phase completion is inaccurate: %v", m.completed)
	}
	rail := ansi.Strip(m.rail())
	if strings.Contains(rail, "[") || !strings.Contains(rail, "✓ Images and assets") || strings.Contains(rail, "✓ Substrate") {
		t.Fatalf("phase rail has bracketed labels or inaccurate completion marks:\n%s", rail)
	}
	m.Update(tea.KeyPressMsg{Code: 'l'})
	m.Update(setupStepMsg("Wait for AKS"))
	view := m.View().Content
	if !strings.Contains(view, "Wait for AKS") || !strings.Contains(view, "Verify saved images") {
		t.Fatalf("activity stopped updating while open:\n%s", view)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.view != "" || m.phase != "cluster" {
		t.Fatal("closing activity lost operation context")
	}
	for range 200 {
		m.Update(setupNoteMsg{text: "bounded activity"})
	}
	if len(m.activity) != 160 || len(m.body) > 12000 {
		t.Fatal("retained activity was unbounded")
	}
	m.Update(tea.KeyPressMsg{Code: 'l'})
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.followActivity || m.contextViewport().AtBottom() {
		t.Fatal("activity could not be scrolled away from the live tail")
	}
}

func TestWorkspaceTinyTerminalCannotAcceptHiddenInput(t *testing.T) {
	m := testWorkspace(t)
	m.Update(tea.WindowSizeMsg{Width: 30, Height: 10})
	workspaceKey(m, tea.KeyEnter)
	if !m.welcome || !strings.Contains(m.View().Content, "Input paused") {
		t.Fatal("tiny terminal accepted hidden input")
	}
}

func TestWorkspaceOversizedPreviewRefusesInsteadOfTruncating(t *testing.T) {
	m := testWorkspace(t)
	err := m.session.preview("test", []installer.ResourceChange{{ChangeType: "Delete", ResourceID: strings.Repeat("x", 1<<20)}})
	if err == nil || !strings.Contains(err.Error(), "no deployment submitted") {
		t.Fatal("oversized destructive preview was silently truncated")
	}
}

func TestWorkspaceDestructivePlanReviewAndNoTerminalInjection(t *testing.T) {
	m := testWorkspace(t)
	m.welcome = false
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	var changes []installer.ResourceChange
	for range 8 {
		changes = append(changes, installer.ResourceChange{ChangeType: "Delete", ResourceID: "/subscriptions/test/resourceGroups/disposable/providers/Microsoft.Storage/storageAccounts/long-name"})
	}
	changes = append(changes, installer.ResourceChange{ChangeType: "Unexpected", ResourceID: "/unknown"})
	m.Update(setupPlanMsg{"test", changes})
	reply := make(chan error, 1)
	m.Update(setupFormMsg{form: huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("Apply?"))), done: reply})
	m.View()
	if !m.reviewRequired || m.reviewed {
		t.Fatal("destructive summary did not require scrolling")
	}
	workspaceKey(m, tea.KeyEnter)
	if m.form == nil {
		t.Fatal("destructive plan approved before review")
	}
	for range 100 {
		workspaceKey(m, tea.KeyPgDown)
		m.View()
	}
	workspaceKey(m, 'a')
	if !m.reviewed || !strings.Contains(m.details, "/unknown") || !strings.Contains(m.body, "WARNING") {
		t.Fatal("review did not retain deletion IDs and unknown changes")
	}
	m.Update(setupPlanMsg{"test", changes})
	if m.reviewed {
		t.Fatal("replacement plan reused an earlier approval acknowledgement")
	}
	m.Update(setupNoteMsg{text: "\x1b[2J\x1b]52;c;bad\x07visible\rtext"})
	for _, line := range m.activity {
		if strings.ContainsAny(line, "\x1b\r\a") {
			t.Fatal("terminal controls reached activity")
		}
	}
}

func TestWorkspaceCancellationWaitsForWorkAndFailureIsNotSuccess(t *testing.T) {
	m := testWorkspace(t)
	m.welcome = false
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd != nil || m.stopping || !m.confirmStop || m.session.ctx.Err() != nil {
		t.Fatal("first Ctrl-C stopped work without confirmation")
	}
	if view := ansi.Strip(m.View().Content); !strings.Contains(view, "Stop setup?") || !strings.Contains(view, "retained") {
		t.Fatalf("stop confirmation was not shown:\n%s", view)
	}
	for _, key := range []tea.KeyPressMsg{{Code: 'x', Text: "x"}, {Code: tea.KeyEnter}} {
		m.Update(key)
		if !m.confirmStop || m.stopping {
			t.Fatalf("%q answered the stop confirmation", key.String())
		}
	}
	for _, key := range []tea.KeyPressMsg{{Code: 'n', Text: "n"}, {Code: tea.KeyEsc}} {
		m.confirmStop = true
		m.Update(key)
		if m.confirmStop || m.stopping || m.session.ctx.Err() != nil {
			t.Fatalf("%q did not dismiss the stop confirmation", key.String())
		}
	}
	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if cmd != nil || !m.stopping || m.confirmStop || m.session.ctx.Err() == nil {
		t.Fatal("confirmed cancellation quit before owned work stopped")
	}
	_, cmd = m.Update(setupFinishedMsg{context.Canceled})
	if cmd == nil {
		t.Fatal("did not exit after cancellation completed")
	}
	m = testWorkspace(t)
	m.welcome = false
	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !m.stopping || m.session.ctx.Err() == nil {
		t.Fatal("a second Ctrl-C did not stop owned work")
	}
	m = testWorkspace(t)
	m.welcome = false
	_, cmd = m.Update(setupFinishedMsg{})
	if cmd == nil || !m.finished {
		t.Fatal("success did not schedule its closing hold")
	}
	if _, ok := cmd().(setupCloseMsg); !ok {
		t.Fatal("success quit without holding the completion screen")
	}
	if view := ansi.Strip(m.View().Content); !strings.Contains(view, "Substrate is running on AKS") ||
		strings.Contains(view, "DONE  Setup complete.") {
		t.Fatalf("success screen did not hand off to the one-time receipt:\n%s", view)
	}
	if _, cmd = m.Update(setupCloseMsg{}); cmd == nil {
		t.Fatal("completion hold did not exit")
	}
	m = testWorkspace(t)
	m.welcome = false
	m.Update(setupPhaseMsg("cluster"))
	failure := errors.New("Azure failed\nResources are retained.\nRetry failed stage: provision")
	_, cmd = m.Update(setupFinishedMsg{failure})
	if cmd != nil || !m.finished || !errors.Is(m.err, failure) {
		t.Fatal("failure did not remain available for inspection")
	}
	if view := m.View().Content; !strings.Contains(view, "Retry failed stage") || strings.Contains(view, "Setup complete") {
		t.Fatalf("failure screen hid recovery or claimed success:\n%s", view)
	}
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil || !errors.Is(m.err, failure) {
		t.Fatal("Ctrl-C could not exit a finished failure screen with its original error")
	}
}

func TestWorkspaceRetainsIdentityWarningAndShowsActiveWorkFirst(t *testing.T) {
	m := testWorkspace(t)
	m.welcome = false
	m.Update(setupNoteMsg{text: "Signed-in identity differs from operator", warning: true})
	m.Update(setupSectionMsg("Image builder"))
	m.Update(setupReviewMsg{{kind: "heading", text: "Installation summary"}})
	if !strings.Contains(m.body, "Signed-in identity differs") {
		t.Fatal("pre-billing identity warning disappeared during review")
	}
	m.Update(setupSectionMsg("Provision"))
	m.Update(setupNoteMsg{text: strings.Repeat("Long context\n", 100)})
	m.Update(setupStepMsg("Waiting for cluster readiness"))
	if !strings.Contains(m.View().Content, "Waiting for cluster readiness") {
		t.Fatal("long context hid current operation feedback")
	}
	m.Update(tea.KeyPressMsg{Code: 'd'})
	if !strings.Contains(m.contextText(), "Installation summary") {
		t.Fatal("installation review was unavailable after moving to another stage")
	}
}

func TestSetupSessionPromptCancellationAndProgressOwnership(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	s := &setupSession{ctx: ctx, events: make(chan any, 10)}
	var terminal, log bytes.Buffer
	p := commandProgress(context.WithValue(ctx, setupSessionKey{}, s), &terminal, &log, "test", true)
	p.step("Named probe")
	p.heading("Heading")
	p.info("Activity")
	p.warn("Warning")
	if _, err := p.Write([]byte("private subprocess diagnostic")); err != nil {
		t.Fatal(err)
	}
	p.complete("Finished")
	p.stop()
	if terminal.Len() != 0 || !strings.Contains(log.String(), "private subprocess diagnostic") {
		t.Fatal("nested progress wrote outside the root renderer or lost diagnostics")
	}
	done := make(chan error, 1)
	go func() { done <- s.prompt(ctx, huh.NewForm(huh.NewGroup(huh.NewConfirm())), nil) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("prompt worker ignored cancellation")
	}
}

func TestWorkspaceIgnoresNavigationFromPreviousForm(t *testing.T) {
	m := testWorkspace(t)
	m.welcome = false
	previous := huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("Previous")))
	current := huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("Approve billable changes?")))
	late := setupFormCommand(previous, func() tea.Msg {
		return tea.BatchMsg{huh.NextField}
	})
	batch := late().(tea.BatchMsg)
	m.Update(setupFormMsg{form: current, done: make(chan error, 1)})
	_, cmd := m.Update(batch[0]())
	if cmd != nil || m.form != current || current.State != huh.StateNormal {
		t.Fatal("late navigation from an old form advanced the new approval")
	}
}

func TestWorkspacePreviewExpandsUnderItsChecklistRow(t *testing.T) {
	m := testWorkspace(t)
	m.welcome = false
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 34})
	m.Update(setupTargetMsg{installer.DefaultConfig(), false})
	m.Update(setupPhaseMsg("images"))
	m.Update(setupChecklistMsg{"Images and assets", []string{"Preview registry bootstrap", "Provision the private registry"}})
	m.Update(setupCheckMsg{name: "Preview registry bootstrap"})
	m.Update(setupPlanMsg{"test", []installer.ResourceChange{
		{ChangeType: "Create", ResourceID: "/subscriptions/test/resourceGroups/example"},
	}})
	m.Update(setupCheckMsg{name: "Preview registry bootstrap", done: true})
	accepted := false
	m.Update(setupFormMsg{form: huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Apply these planned changes?").Value(&accepted))), done: make(chan error, 1)})
	if m.title != "Images and assets" {
		t.Fatalf("preview replaced the phase checklist title: %q", m.title)
	}
	view := ansi.Strip(m.View().Content)
	order := []string{"✓ Preview registry bootstrap", "Review registry changes", "1 to create", "○ Provision the private registry", "Apply these planned"}
	last := -1
	for _, want := range order {
		at := strings.Index(view, want)
		if at <= last {
			t.Fatalf("inline preview is missing or out of order at %q:\n%s", want, view)
		}
		last = at
	}
	for _, line := range strings.Split(view, "\n") {
		if ansi.StringWidth(line) > 110 {
			t.Fatalf("inline preview overflowed the terminal:\n%s", view)
		}
	}
	m.form, m.reply = nil, nil
	m.Update(setupCheckMsg{name: "Provision the private registry"})
	if m.plan || strings.Contains(ansi.Strip(m.View().Content), "Review registry changes") {
		t.Fatal("approved preview stayed expanded after the next check started")
	}
}

func TestWorkspaceTimersPauseWhileWaitingOnInput(t *testing.T) {
	m := testWorkspace(t)
	m.welcome = false
	m.workStarted = m.clock()
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	m.Update(setupChecklistMsg{"Check your environment", []string{"Image builder"}})
	m.Update(setupCheckMsg{name: "Image builder"})
	m.Update(setupFormMsg{form: huh.NewForm(huh.NewGroup(huh.NewInput().Title("Engine"))), done: make(chan error, 1)})
	frozen := m.clock()
	time.Sleep(30 * time.Millisecond)
	if !m.clock().Equal(frozen) {
		t.Fatal("work clock advanced while setup waited on the user")
	}
	if view := ansi.Strip(m.View().Content); !strings.Contains(view, "paused for input") || !strings.Contains(view, "waiting for you") {
		t.Fatalf("waiting state was not visible:\n%s", view)
	}
	m.form, m.reply = nil, nil
	m.Update(setupTickMsg{})
	if m.waiting() || m.paused < 30*time.Millisecond || m.clock().Before(frozen) {
		t.Fatalf("clock did not resume without the paused time: paused=%v", m.paused)
	}
}

func TestWorkspaceHeaderPrefersSubscriptionName(t *testing.T) {
	m := testWorkspace(t)
	c := installer.DefaultConfig()
	c.Azure.SubscriptionID = "11111111-2222-3333-4444-555555555555"
	m.Update(setupTargetMsg{c, false})
	m.Update(setupSubscriptionMsg{"99999999-2222-3333-4444-555555555555", "Someone Else"})
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	if header := ansi.Strip(m.headerView()); strings.Contains(header, "Someone Else") || !strings.Contains(header, c.Azure.SubscriptionID) {
		t.Fatalf("header named a different subscription: %q", header)
	}
	m.Update(setupSubscriptionMsg{strings.ToUpper(c.Azure.SubscriptionID), "Contoso Platform Engineering Dev"})
	if header := ansi.Strip(m.headerView()); !strings.Contains(header, "subscription Contoso Platform Engineering Dev") {
		t.Fatalf("header did not prefer the subscription name: %q", header)
	}
	m.Update(tea.WindowSizeMsg{Width: 72, Height: 30})
	if header := ansi.Strip(m.headerView()); !strings.Contains(header, "Contoso Platfor…") || ansi.StringWidth(header) > 72 {
		t.Fatalf("narrow header did not shorten the subscription name: %q", header)
	}
}

func TestWorkspaceLogsViewFollowsTheDiagnosticLog(t *testing.T) {
	path := t.TempDir() + "/setup.log"
	write := func(text string) {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if _, err := file.WriteString(text); err != nil {
			t.Fatal(err)
		}
	}
	write("[command] plan\n$ \"az\" \"group\" \"show\" \"--name\" \"my rg\"\n{\"id\": \"x\"}\x1b]0;title\x07\x1b[31mred\x1b[0m\r\n$ \"az\" \"\\x1bc\"\n")
	m := testWorkspace(t)
	m.welcome = false
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	m.Update(setupLogMsg(path))
	workspaceKey(m, 'l')
	view := m.View().Content
	plain := ansi.Strip(view)
	for _, want := range []string{"LOGS", "Following", path, `$ az group show --name "my rg"`, `{"id": "x"}`, "red", `$ az "\x1bc"`} {
		if !strings.Contains(plain, want) {
			t.Fatalf("logs view lost %q:\n%s", want, plain)
		}
	}
	if strings.Contains(view, "\x1b]0;") || strings.Contains(view, "\x1b[31m") || strings.Contains(view, "\x1bc") {
		t.Fatalf("logs view passed through subprocess terminal controls: %q", view)
	}
	write("\n[step] Creating the registry\n")
	m.Update(setupTickMsg{})
	if plain := ansi.Strip(m.View().Content); !strings.Contains(plain, "▸ Creating the registry") {
		t.Fatalf("logs view did not follow new output:\n%s", plain)
	}
	write(strings.Repeat("filler output line\n", setupLogTailBytes/10))
	m.Update(setupTickMsg{})
	if !strings.Contains(m.logTail, "Earlier output omitted") || len(m.logTail) > 4*setupLogTailBytes {
		t.Fatal("logs view did not bound a large log")
	}
}

func TestWorkspacePreviewCountsChildResourcesUnderTheirParent(t *testing.T) {
	prefix := "/subscriptions/test/resourceGroups/rg/providers/"
	storage := prefix + "Microsoft.Storage/storageAccounts/assets"
	identity := prefix + "Microsoft.ManagedIdentity/userAssignedIdentities/"
	changes := []installer.ResourceChange{
		{ChangeType: "Create", ResourceID: storage},
		{ChangeType: "Create", ResourceID: storage + "/blobServices/default"},
		{ChangeType: "Create", ResourceID: storage + "/blobServices/default/containers/snapshots"},
		{ChangeType: "Create", ResourceID: storage + "/blobServices/default/containers/assets"},
		{ChangeType: "Create", ResourceID: identity + "node"},
		{ChangeType: "Create", ResourceID: identity + "node/federatedIdentityCredentials/node"},
	}
	m := testWorkspace(t)
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 32})
	lines := strings.Split(ansi.Strip(m.planView(changes, true)), "\n")
	var rows []string
	for _, line := range lines {
		if line = strings.Join(strings.Fields(line), " "); strings.Contains(line, "create") && !strings.Contains(line, "to create") {
			rows = append(rows, line)
		}
	}
	want := []string{"Managed identity 1 create", "Federated credentials 1 create",
		"Storage account 1 create", "Blob containers 2 create", "Blob service 1 create"}
	if !slices.Equal(rows, want) {
		t.Fatalf("child resources were not listed under their parent:\ngot  %q\nwant %q", rows, want)
	}
}

func TestWorkspaceLogPreviewSurvivesBinaryOutput(t *testing.T) {
	path := t.TempDir() + "/setup.log"
	// 0x90 is an 8-bit DCS introducer that never terminates.
	log := "$ \"zstd\"\nsubstrate\x90\x1b[31m\x9b\x00probe\n[step] Azure deployment readiness\n$ \"az\" \"account\" \"show\"\n"
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	m := testWorkspace(t)
	m.welcome = false
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 34})
	m.Update(setupChecklistMsg{"Preparation", []string{"Azure deployment readiness"}})
	m.Update(setupCheckMsg{name: "Azure deployment readiness"})
	m.Update(setupLogMsg(path))
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "$ az account show") || !strings.Contains(view, "Azure deployment readiness") {
		t.Fatalf("binary bytes hid later log lines:\n%s", view)
	}
}

func TestWorkspaceRunningChecklistPreviewsTheLogTail(t *testing.T) {
	path := t.TempDir() + "/setup.log"
	var log strings.Builder
	for i := range 60 {
		fmt.Fprintf(&log, "output line %02d\n\n", i)
	}
	if err := os.WriteFile(path, []byte(log.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	m := testWorkspace(t)
	m.welcome = false
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 34})
	m.Update(setupChecklistMsg{"AKS cluster", []string{"Provision AKS"}})
	m.Update(setupCheckMsg{name: "Provision AKS"})
	if strings.Contains(ansi.Strip(m.View().Content), "── log") {
		t.Fatal("log preview appeared before any command log existed")
	}
	m.Update(setupLogMsg(path))
	view := ansi.Strip(m.View().Content)
	_, tail, found := strings.Cut(view, "── log")
	if !found || !strings.Contains(tail, "l expand") || !strings.Contains(tail, "output line 59") ||
		strings.Contains(tail, "output line 00") || strings.Contains(tail, "\n\n") {
		t.Fatalf("running checklist did not preview the last non-blank log lines:\n%s", view)
	}
	if shown := strings.Count(tail, "output line"); shown < 15 || lipgloss.Height(view) > 34 {
		t.Fatalf("log preview should fill the free body rows without overflowing (%d lines shown):\n%s", shown, view)
	}
}
