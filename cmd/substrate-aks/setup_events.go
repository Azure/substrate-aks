package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/Azure/substrate-aks/internal/installer"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

// Only guided setup attaches a session. Standalone commands keep their existing
// presenter, logging, locks and command semantics.
type setupSessionKey struct{}

type setupSession struct {
	ctx    context.Context
	events chan any
	// ui and receipt are built before the workspace starts: building a
	// renderer for a terminal queries it, and the running workspace would
	// consume the reply, blocking the query forever.
	ui, receipt *setupUI
}

func setupSessionFrom(ctx context.Context) *setupSession {
	s, _ := ctx.Value(setupSessionKey{}).(*setupSession)
	return s
}

func (s *setupSession) send(event any) {
	select {
	case s.events <- event:
	case <-s.ctx.Done():
	}
}

type setupSectionMsg string
type setupNoteMsg struct {
	text    string
	warning bool
}
type setupReviewEntry struct {
	kind, label, text, detail string
}
type setupReviewMsg []setupReviewEntry
type setupTargetMsg struct {
	config installer.Config
	demo   bool
}

// setupSubscriptionMsg names a subscription once Azure discovery has read it.
type setupSubscriptionMsg struct{ id, name string }
type setupPhaseMsg string
type setupStepMsg string
type setupChecklistMsg struct {
	title string
	items []string
}
type setupCheckMsg struct {
	name string
	done bool
	err  error
}
type setupCheckGroupMsg struct {
	names []string
	done  bool
	err   error
}
type setupSyncMsg chan struct{}
type setupLogMsg string
type setupPlanMsg struct {
	subscription string
	changes      []installer.ResourceChange
}
type setupFormMsg struct {
	form   *huh.Form
	done   chan error
	resize func(height int)
}

func useSetupWorkspace(o setupOptions, in io.Reader, out io.Writer) bool {
	if o.yes || o.accessible || os.Getenv("TERM") == "dumb" {
		return false
	}
	input, ok := in.(*os.File)
	if !ok || !term.IsTerminal(input.Fd()) {
		return false
	}
	output, ok := out.(*os.File)
	return ok && term.IsTerminal(output.Fd())
}

func setupPhase(command string, flags []string) string {
	switch command {
	case "plan", "provision":
		for _, flag := range flags {
			if flag == "--registry-only" {
				return "images"
			}
		}
		return "cluster"
	case "images":
		return "images"
	case "check":
		return "cluster"
	case "prepare", "install", "status":
		return "substrate"
	case "demo":
		return "demo"
	case "client":
		return "connect"
	default:
		return ""
	}
}

func setupStageChecks(label, command string, flags []string) []string {
	if command == "images" && len(flags) > 0 && flags[0] == "--check" {
		return installer.ImageCheckSteps()
	}
	return []string{label}
}

func setupPhases(c installer.Config, withDemo bool) []string {
	phases := []string{"prepare"}
	for _, stage := range setupStages(c, withDemo) {
		phase := setupPhase(stage.command, stage.flags)
		if !slices.Contains(phases, phase) {
			phases = append(phases, phase)
		}
	}
	return phases
}

func (s *setupSession) prompt(ctx context.Context, form *huh.Form, resize func(height int)) error {
	done := make(chan error, 1)
	s.send(setupFormMsg{form: form, done: done, resize: resize})
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *setupSession) preview(subscription string, changes []installer.ResourceChange) error {
	size := 0
	for _, change := range changes {
		size += len(change.ResourceID) + len(change.ChangeType) + 32
	}
	if size > 1<<20 {
		return errors.New("resource preview exceeds the interactive review limit; no deployment submitted. Inspect the private diagnostic log and use standalone plan/provision for this installation")
	}
	s.send(setupPlanMsg{subscription, changes})
	return s.ctx.Err()
}

// Raw subprocess output belongs in the command's private log, never in progress
// messages. Progress shows explicit steps and structured previews; the logs
// view reads a sanitized tail of the log file instead.
func commandProgress(ctx context.Context, out, log io.Writer, stage string, animate bool) *progress {
	s := setupSessionFrom(ctx)
	if s == nil {
		return newProgress(out, log, stage, animate)
	}
	stopped := make(chan struct{})
	close(stopped)
	return &progress{out: out, log: log, console: newConsole(out), stage: stage,
		started: time.Now(), done: make(chan struct{}), stopped: stopped, session: s}
}

var setupPlanActions = []struct{ name, heading, label, color string }{
	{"Create", "to create", "create", "2"}, {"Modify", "to update", "update", "3"},
	{"Delete", "to delete", "delete", "1"}, {"Deploy", "to deploy", "deploy", setupAccent},
	{"Unknown", "unrecognized", "unrecognized", "3"},
}

var setupResourceNames = map[string]string{
	"Microsoft.Authorization/roleAssignments":          "Role assignments",
	"Microsoft.ContainerRegistry/registries":           "Container registry",
	"Microsoft.ContainerService/managedClusters":       "AKS cluster",
	"Microsoft.Storage/storageAccounts":                "Storage account",
	"Microsoft.ManagedIdentity/userAssignedIdentities": "Managed identity",
	"Microsoft.Network/virtualNetworks":                "Virtual network",
	"Microsoft.Network/privateEndpoints":               "Private endpoint",
	"Microsoft.Network/privateDnsZones":                "Private DNS zone",
}

var setupChildNames = map[string]string{
	"blobServices":                 "Blob service",
	"blobServices/containers":      "Blob containers",
	"federatedIdentityCredentials": "Federated credentials",
	"agentPools":                   "Node pools",
}

// setupResource is a readable view of an ARM resource ID: its top-level kind,
// an optional child kind, its resource group, and the names along its path.
type setupResource struct {
	kind, child, group string
	// scope names the resource an extension such as a role assignment is on.
	scope string
	names []string
}

func parseSetupResource(id string) setupResource {
	parts := strings.Split(strings.Trim(safeSetupText(id), "/"), "/")
	ref := setupResource{kind: "Other resources"}
	for i, part := range parts {
		if strings.EqualFold(part, "resourceGroups") && i+1 < len(parts) && ref.group == "" {
			ref.group = parts[i+1]
		}
		if strings.EqualFold(part, "providers") && i+2 < len(parts) {
			// Provider paths alternate type and name after the namespace.
			var types, names []string
			for j := i + 2; j < len(parts) && !strings.EqualFold(parts[j], "providers"); j += 2 {
				types = append(types, parts[j])
				if j+1 < len(parts) {
					names = append(names, parts[j+1])
				}
			}
			if len(ref.names) > 0 {
				ref.scope = ref.names[len(ref.names)-1]
			}
			ref.kind, ref.child, ref.names = parts[i+1]+"/"+types[0], strings.Join(types[1:], "/"), names
		}
	}
	if ref.kind == "Other resources" && len(parts) == 4 && strings.EqualFold(parts[2], "resourceGroups") {
		ref.kind, ref.names = "Resource group", []string{parts[3]}
	}
	if name, ok := setupResourceNames[ref.kind]; ok {
		ref.kind = name
	}
	if name, ok := setupChildNames[ref.child]; ok {
		ref.child = name
	}
	return ref
}

// planDetails lists every change in a preview, grouped by action, for the
// details view. Full IDs are kept only where a reviewer must verify them.
func (m *setupWorkspace) planDetails(subscription string, changes []installer.ResourceChange) string {
	st, width := m.st, m.bodyWidth()
	subscription = safeSetupText(subscription)
	groups := map[string]int{}
	for _, change := range changes {
		if group := parseSetupResource(change.ResourceID).group; group != "" {
			groups[group]++
		}
	}
	primary := ""
	for group, count := range groups {
		if count > groups[primary] || count == groups[primary] && group < primary {
			primary = group
		}
	}
	lines := []string{st.bold.Render("Planned resource changes"), st.muted.Render("Subscription    ") + subscription}
	if primary != "" {
		lines = append(lines, st.muted.Render("Resource group  ")+primary)
	}
	byAction := map[string][]installer.ResourceChange{}
	unchanged := 0
	for _, change := range changes {
		action := change.ChangeType
		switch action {
		case "NoChange":
			unchanged++
			continue
		case "Create", "Modify", "Delete", "Deploy":
		default:
			action = "Unknown"
		}
		byAction[action] = append(byAction[action], change)
	}
	kindWidth := min(24, max(12, width/3))
	for _, action := range setupPlanActions {
		list := byAction[action.name]
		if len(list) == 0 {
			continue
		}
		color := m.renderer.NewStyle().Foreground(lipgloss.Color(action.color))
		heading := fmt.Sprintf("%d %s ", len(list), action.heading)
		lines = append(lines, "", color.Bold(true).Render(heading)+st.muted.Render(strings.Repeat("─", max(0, width-ansi.StringWidth(heading)))))
		refs := make([]setupResource, len(list))
		for i, change := range list {
			refs[i] = parseSetupResource(change.ResourceID)
		}
		order := make([]int, len(list))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool {
			x, y := refs[order[a]], refs[order[b]]
			if x.kind != y.kind {
				if x.kind == "Resource group" || y.kind == "Resource group" {
					return x.kind == "Resource group"
				}
				return x.kind < y.kind
			}
			if x.child != y.child {
				return x.child < y.child
			}
			return strings.Join(x.names, "/") < strings.Join(y.names, "/")
		})
		symbol, _ := planChange(action.name)
		for _, i := range order {
			ref, change := refs[i], list[i]
			id := relativeResourceID(subscription, safeSetupText(change.ResourceID))
			kind, kindStyle := ref.kind, m.renderer.NewStyle()
			if ref.child != "" {
				kind, kindStyle = "  "+ref.child, st.muted
			}
			name := strings.Join(ref.names, " › ")
			if name == "" {
				name = id
			}
			showID := (action.name == "Delete" || action.name == "Unknown") && name != id
			if ref.scope != "" {
				name += st.muted.Render("  on " + ref.scope)
			}
			switch {
			case ref.group != "" && ref.group != primary:
				name += st.muted.Render("  in " + ref.group)
			case ref.group == "" && len(ref.names) > 0:
				name += st.muted.Render("  subscription scope")
			}
			if action.name == "Unknown" {
				name += m.renderer.NewStyle().Foreground(lipgloss.Color(setupWarn)).Render("  (" + safeSetupText(change.ChangeType) + ")")
			}
			nameWidth := max(1, width-kindWidth-3)
			lines = append(lines, lipgloss.JoinHorizontal(lipgloss.Top,
				color.Render(symbol[:1])+" ",
				kindStyle.Width(kindWidth).Render(ansi.Truncate(kind, kindWidth-1, "…")), " ",
				m.renderer.NewStyle().Width(nameWidth).Render(ansi.Wrap(name, nameWidth, "/-"))))
			if showID {
				lines = append(lines, st.muted.Render(ansi.Hardwrap("  "+id, width, true)))
			}
		}
	}
	if len(byAction) == 0 {
		lines = append(lines, "", st.bold.Render("No resource changes"))
	}
	if unchanged > 0 {
		lines = append(lines, "", st.muted.Render(fmt.Sprintf("%d unchanged", unchanged)))
	}
	lines = append(lines, "", st.muted.Render(ansi.Wrap("Preview only. Nothing has been applied. Property-level changes are in the diagnostic log (press l).", width, "")))
	return strings.Join(lines, "\n")
}

// planView summarizes a preview. Inline previews sit in the checklist callout,
// which supplies its own frame and is four columns narrower than the body.
func (m *setupWorkspace) planView(changes []installer.ResourceChange, inline bool) string {
	width := min(m.bodyWidth(), 88)
	boxed := width >= 60 && m.height >= 24 && !inline
	if inline {
		width = max(1, min(m.bodyWidth()-4, 84))
	}
	innerWidth := width
	if boxed {
		innerWidth -= 6
	}
	muted := m.renderer.NewStyle().Foreground(lipgloss.Color("8"))
	counts := map[string]int{}
	for _, change := range changes {
		counts[change.ChangeType]++
	}
	actions := setupPlanActions
	counts["Unknown"] = len(changes) - counts["Create"] - counts["Modify"] - counts["Delete"] - counts["Deploy"] - counts["NoChange"]
	var totals []string
	for _, kind := range actions {
		if count := counts[kind.name]; count > 0 {
			totals = append(totals, m.renderer.NewStyle().Bold(true).Foreground(lipgloss.Color(kind.color)).
				Render(fmt.Sprintf("%d %s", count, kind.heading)))
		}
	}
	headline := strings.Join(totals, "   ")
	if headline == "" {
		headline = m.renderer.NewStyle().Bold(true).Render("No resource changes")
	}
	lines := []string{ansi.Wrap(headline, innerWidth, "")}
	// Group by top-level resource; child resources (blob containers, federated
	// credentials) get their own indented rows so parent counts stay literal.
	type row struct{ kind, child string }
	resources := map[row]map[string]int{}
	for _, change := range changes {
		if change.ChangeType == "NoChange" {
			continue
		}
		ref := parseSetupResource(change.ResourceID)
		key := row{kind: ref.kind, child: ref.child}
		if resources[key] == nil {
			resources[key] = map[string]int{}
		}
		if key.child != "" && resources[row{kind: key.kind}] == nil {
			resources[row{kind: key.kind}] = map[string]int{}
		}
		action := change.ChangeType
		switch action {
		case "Create", "Modify", "Delete", "Deploy":
		default:
			action = "Unknown"
		}
		resources[key][action]++
	}
	rows := make([]row, 0, len(resources))
	for key := range resources {
		rows = append(rows, key)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.kind != b.kind {
			if a.kind == "Resource group" || b.kind == "Resource group" {
				return a.kind == "Resource group"
			}
			return a.kind < b.kind
		}
		return a.child < b.child
	})
	if len(rows) > 0 {
		lines = append(lines, "")
	}
	for _, key := range rows {
		var labels []string
		for _, action := range actions {
			if count := resources[key][action.name]; count > 0 {
				labels = append(labels, m.renderer.NewStyle().Foreground(lipgloss.Color(action.color)).
					Render(fmt.Sprintf("%d %s", count, action.label)))
			}
		}
		summary := strings.Join(labels, "  ")
		label, labelStyle := key.kind, m.renderer.NewStyle()
		if key.child != "" {
			label, labelStyle = "  "+key.child, muted
		}
		labelWidth := min(26, innerWidth/2)
		if ansi.StringWidth(summary) > innerWidth-labelWidth-2 {
			lines = append(lines, labelStyle.Render(ansi.Hardwrap(label, innerWidth, true)),
				"    "+ansi.Wrap(summary, innerWidth-4, ""))
			continue
		}
		lines = append(lines, lipgloss.JoinHorizontal(lipgloss.Top,
			labelStyle.Width(labelWidth).Render(label),
			"  ", summary))
	}
	if counts["NoChange"] > 0 {
		lines = append(lines, "", muted.Render(fmt.Sprintf("%d unchanged", counts["NoChange"])))
	}
	content := strings.Join(lines, "\n")
	if boxed {
		content = m.renderer.NewStyle().Width(width-2).Padding(0, 2).
			Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(setupAccent)).Render(content)
	}
	content += "\n\n" + muted.Render(ansi.Wrap("Preview only. These changes have not been applied.", width, ""))
	for _, change := range changes {
		switch change.ChangeType {
		case "Delete":
			content += "\n\n" + m.renderer.NewStyle().Foreground(lipgloss.Color("1")).
				Render("DELETE "+safeSetupText(change.ResourceID))
		case "Create", "Modify", "Deploy", "NoChange":
		default:
			content += "\n\n" + m.renderer.NewStyle().Foreground(lipgloss.Color("3")).
				Render(fmt.Sprintf("WARNING: unknown change %q: %s", safeSetupText(change.ChangeType), safeSetupText(change.ResourceID)))
		}
	}
	return content
}

var errSetupFormAborted = errors.New("setup input cancelled")
