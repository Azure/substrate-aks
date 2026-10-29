package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/Azure/substrate-aks/internal/installer"
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
	for _, size := range [][2]int{{120, 40}, {100, 30}, {80, 24}, {79, 24}, {64, 34}, {40, 26}, {40, 24}, {80, 14}, {40, 14}} {
		for _, choice := range []int{0, 1} {
			m := testWorkspace(t)
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			m.choice = choice
			view := ansi.Strip(m.View().Content)
			for _, want := range []string{"Quick start", "Advanced configuration", "[Planned]", "cannot start", "Ctrl-C"} {
				if !strings.Contains(view, want) {
					t.Fatalf("%dx%d missing %q:\n%s", size[0], size[1], want, view)
				}
			}
			if strings.Contains(view, "Existing AKS cluster") {
				t.Fatal("removed option is still advertised")
			}
			if size[0] >= 80 && size[1] >= 24 {
				for _, want := range []string{"/___________/|", "s u b s t r a t e", "on AKS"} {
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
	if strings.Contains(view, "> ") || !strings.Contains(view, "│  Quick start") ||
		!strings.Contains(view, "│  Advanced configuration") || !strings.Contains(view, "╭─") {
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
			!strings.HasPrefix(lines[2], "  Long probe detail") || ansi.StringWidth(lines[2]) > m.bodyWidth() ||
			!strings.Contains(lines[3], "○ API access") {
			t.Fatalf("active status was not bounded beneath its owning check: %q", m.contextText())
		}
	}
	failure := errors.New("Azure session expired")
	m.Update(setupCheckMsg{name: "Azure account", done: true, err: failure})
	if strings.Contains(m.contextText(), "Long probe detail") {
		t.Fatal("completed check retained its live status line")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyF3})
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
	m.Update(setupStepMsg("Loading regional sizes"))
	column := func(line, text string) int {
		index := strings.Index(line, text)
		if index < 0 {
			return -1
		}
		return ansi.StringWidth(line[:index])
	}
	for range m.checkSpinner.Spinner.Frames {
		lines := strings.Split(ansi.Strip(m.checklistView()), "\n")
		start := column(lines[0], "Local tools")
		if start != column(lines[1], "Azure readiness") || start != column(lines[2], "Loading regional sizes") ||
			start != column(lines[3], "Image builder") {
			t.Fatalf("spinner shifted text away from the check/detail column:\n%s", strings.Join(lines, "\n"))
		}
		m.checkSpinner, _ = m.checkSpinner.Update(m.checkSpinner.Tick())
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
	expected := map[string]int{"registry": 2, "images": 2, "cluster": 3, "substrate": 3, "demo": 1, "connect": 1}
	seen := map[string]string{}
	err := setupForTest(ctx, setupOptions{
		path: testCLIConfig(t), yes: true, withDemo: true, skipContextHandoff: true,
		discovery: subscriptionDisplayRunner(t),
	}, strings.NewReader(""), io.Discard, io.Discard,
		func(context.Context, []string, io.Writer, io.Writer) error {
			drain()
			if len(m.checks) != expected[m.phase] || len(m.checks) == 0 {
				t.Fatalf("phase %s did not declare its operations before execution: %+v", m.phase, m.checks)
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
	m.Update(setupChecklistMsg{"Private registry", items})
	m.Update(setupCheckMsg{name: items[0]})
	for _, detail := range []string{"", "Checking Azure subscription access", "Preparing infrastructure templates"} {
		if detail != "" {
			m.Update(setupStepMsg(detail))
		}
		m.Update(setupNoteMsg{text: "Target: redundant / eastus2"})
		text := ansi.Strip(m.contextText())
		lines := strings.Split(text, "\n")
		if len(lines) != 3 || !strings.Contains(lines[0], items[0]) ||
			!strings.Contains(lines[2], "○ "+items[1]) || strings.Contains(text, "[work]") ||
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
	if m.plan || m.title != "Private registry" || !strings.Contains(text, "✓ "+items[0]) ||
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
	m.Update(setupFormMsg{huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Apply these planned changes?").Value(&value))), make(chan error, 1)})
	if m.scroll != 0 || !m.reviewRequired || m.contextViewport().AtBottom() {
		t.Fatal("preserved checklist scrolled a destructive review directly to the bottom")
	}
	workspaceKey(m, tea.KeyF4)
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
			!strings.Contains(ansi.Strip(m.body), "WARNING:") || strings.Contains(m.body, "\x1b]") {
			t.Fatalf("review lost content or allowed terminal control: %q", m.body)
		}
		value := false
		m.Update(setupFormMsg{huh.NewForm(huh.NewGroup(
			huh.NewConfirm().Title("Continue with this billable installation?").Value(&value))),
			make(chan error, 1)})
		coloredReview := strings.Contains(m.body, "\x1b[")
		beforeTitle, _, found := strings.Cut(m.form.View(), "Continue with")
		if !found {
			t.Fatal("prompt title disappeared")
		}
		coloredPrompt := strings.Contains(beforeTitle, "\x1b[38;")
		if profile == termenv.TrueColor && !strings.Contains(beforeTitle, "180;140;242") {
			t.Fatalf("embedded prompt did not preserve the purple brand color: %q", beforeTitle)
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
		m.Update(setupPhaseMsg("registry"))
		m.Update(setupPlanMsg{"subscription", []installer.ResourceChange{{ChangeType: "Create", ResourceID: "/subscriptions/subscription/resourceGroups/test-install"}}})
		value := false
		m.Update(setupFormMsg{huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("Continue with this billable installation?").Value(&value))), make(chan error, 1)})
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := ansi.Strip(m.View().Content)
		for _, want := range []string{"test-install", "Continue with this", "Yes", "No", "Ctrl-C"} {
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
		m.Update(setupFormMsg{huh.NewForm(huh.NewGroup(
			huh.NewSelect[string]().Title("Choose an image builder").Options(
				huh.NewOption("Docker", "docker"), huh.NewOption("Podman", "podman")).Value(&engine))),
			make(chan error, 1)})
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

func TestWorkspaceHeaderStacksTargetBesideBrand(t *testing.T) {
	for _, width := range []int{120, 96, 80, 40} {
		m := testWorkspace(t)
		m.welcome = false
		m.Update(tea.WindowSizeMsg{Width: width, Height: 32})
		c := installer.DefaultConfig()
		c.AKS.ClusterName, c.Azure.Location = "substrate-dev-tui", "eastus2"
		c.Azure.SubscriptionID = "11111111-1111-4111-8111-111111111111"
		m.Update(setupTargetMsg{c, false})
		m.Update(setupFormMsg{huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("Continue?"))), make(chan error, 1)})
		header := ansi.Strip(m.headerView())
		if !strings.Contains(header, "Agent Substrate on AKS") || strings.Contains(header, "Elapsed") ||
			strings.Contains(header, "DECISION") || strings.Contains(header, "WORKING") {
			t.Fatalf("header retained status noise or lost branding:\n%s", header)
		}
		if width >= 80 {
			lines := strings.Split(header, "\n")
			if len(lines) != 3 || !strings.Contains(lines[0], c.AKS.ClusterName) ||
				!strings.Contains(lines[1], "eastus2") || !strings.HasSuffix(lines[2], c.Azure.SubscriptionID) ||
				strings.Index(lines[0], "Cluster") != strings.Index(lines[1], "Region") ||
				strings.Index(lines[1], "Region") != strings.Index(lines[2], "Subscription") {
				t.Fatalf("target metadata is not a right-aligned stack:\n%s", header)
			}
		}
		for _, line := range strings.Split(ansi.Strip(m.View().Content), "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatalf("header or body overflowed width %d:\n%s", width, line)
			}
		}
		if width >= 80 {
			m.cluster = strings.Repeat("x", 63)
			header = ansi.Strip(m.headerView())
			if !strings.Contains(header, c.Azure.SubscriptionID) {
				t.Fatalf("long cluster name displaced the subscription:\n%s", header)
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
		m.Update(setupPhaseMsg("registry"))
		m.Update(setupPlanMsg{"test", []installer.ResourceChange{
			{ChangeType: "Create", ResourceID: prefix},
			{ChangeType: "Create", ResourceID: prefix + "/providers/Microsoft.ContainerRegistry/registries/example"},
			{ChangeType: "Create", ResourceID: prefix + "/providers/Microsoft.Authorization/roleAssignments/example"},
		}})
		m.Update(setupNoteMsg{text: "Infrastructure preview complete (22s)."})
		accepted := false
		m.Update(setupFormMsg{huh.NewForm(huh.NewGroup(
			huh.NewConfirm().Title("Apply these planned changes?").Value(&accepted))), make(chan error, 1)})
		body := ansi.Strip(m.body)
		for _, want := range []string{"3 to create", "Resource group", "Container registry", "Access grants"} {
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
		for _, want := range []string{"Review registry changes", "Apply these planned", "Yes", "No", "F2"} {
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
	m.Update(tea.KeyPressMsg{Code: tea.KeyF3})
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
	m.Update(tea.KeyPressMsg{Code: tea.KeyF3})
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
	m.Update(setupFormMsg{huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("Apply?"))), reply})
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
	workspaceKey(m, tea.KeyF4)
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
	if cmd != nil || !m.stopping || m.session.ctx.Err() == nil {
		t.Fatal("cancellation quit before owned work stopped")
	}
	_, cmd = m.Update(setupFinishedMsg{context.Canceled})
	if cmd == nil {
		t.Fatal("did not exit after cancellation completed")
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
	m.Update(tea.KeyPressMsg{Code: tea.KeyF2})
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
	go func() { done <- s.prompt(ctx, huh.NewForm(huh.NewGroup(huh.NewConfirm()))) }()
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
	m.Update(setupFormMsg{current, make(chan error, 1)})
	_, cmd := m.Update(batch[0]())
	if cmd != nil || m.form != current || current.State != huh.StateNormal {
		t.Fatal("late navigation from an old form advanced the new approval")
	}
}
