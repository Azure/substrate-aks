package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	kind, label, text string
}
type setupReviewMsg []setupReviewEntry
type setupTargetMsg struct {
	config installer.Config
	demo   bool
}
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
type setupSyncMsg chan struct{}
type setupLogMsg string
type setupPlanMsg struct {
	subscription string
	changes      []installer.ResourceChange
}
type setupFormMsg struct {
	form *huh.Form
	done chan error
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
				return "registry"
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

func (s *setupSession) prompt(ctx context.Context, form *huh.Form) error {
	done := make(chan error, 1)
	s.send(setupFormMsg{form, done})
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

// Raw subprocess output belongs in the command's private log, never in the
// interactive renderer. Display only explicit progress and structured previews.
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

func (m *setupWorkspace) planView(changes []installer.ResourceChange) string {
	width := min(m.bodyWidth(), 88)
	boxed := width >= 60 && m.height >= 24
	innerWidth := width
	if boxed {
		innerWidth -= 6
	}
	muted := m.renderer.NewStyle().Foreground(lipgloss.Color("8"))
	counts := map[string]int{}
	for _, change := range changes {
		counts[change.ChangeType]++
	}
	var totals []string
	for _, kind := range []struct{ name, label, color string }{
		{"Create", "to create", "2"}, {"Modify", "to update", "3"},
		{"Delete", "to delete", "1"}, {"Deploy", "to deploy", setupAccent},
	} {
		if count := counts[kind.name]; count > 0 {
			totals = append(totals, m.renderer.NewStyle().Bold(true).Foreground(lipgloss.Color(kind.color)).
				Render(fmt.Sprintf("%d %s", count, kind.label)))
		}
	}
	unknown := len(changes) - counts["Create"] - counts["Modify"] - counts["Delete"] - counts["Deploy"] - counts["NoChange"]
	if unknown > 0 {
		totals = append(totals, m.renderer.NewStyle().Foreground(lipgloss.Color("3")).
			Render(fmt.Sprintf("%d unrecognized", unknown)))
	}
	headline := strings.Join(totals, "   ")
	if headline == "" {
		headline = m.renderer.NewStyle().Bold(true).Render("No resource changes")
	}
	lines := []string{ansi.Wrap(headline, innerWidth, "")}
	resources := map[string]int{}
	names := map[string]string{
		"Microsoft.Authorization/roleAssignments":          "Access grants",
		"Microsoft.ContainerRegistry/registries":           "Container registry",
		"Microsoft.ContainerService/managedClusters":       "AKS cluster",
		"Microsoft.Storage/storageAccounts":                "Storage account",
		"Microsoft.ManagedIdentity/userAssignedIdentities": "Managed identity",
		"Microsoft.Network/virtualNetworks":                "Virtual network",
		"Microsoft.Network/privateEndpoints":               "Private endpoint",
		"Microsoft.Network/privateDnsZones":                "Private DNS zone",
	}
	for _, change := range changes {
		if change.ChangeType == "NoChange" {
			continue
		}
		parts := strings.Split(strings.Trim(safeSetupText(change.ResourceID), "/"), "/")
		kind := "Other resources"
		for i, part := range parts {
			if strings.EqualFold(part, "providers") && i+2 < len(parts) {
				kind = parts[i+1] + "/" + parts[i+2]
			}
		}
		if kind == "Other resources" && len(parts) == 4 && strings.EqualFold(parts[2], "resourceGroups") {
			kind = "Resource group"
		}
		if name, ok := names[kind]; ok {
			kind = name
		}
		resources[kind]++
	}
	kinds := make([]string, 0, len(resources))
	for kind := range resources {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool {
		if kinds[i] == kinds[j] {
			return false
		}
		if kinds[i] == "Resource group" {
			return true
		}
		if kinds[j] == "Resource group" {
			return false
		}
		return kinds[i] < kinds[j]
	})
	if len(kinds) > 0 {
		lines = append(lines, "")
	}
	for _, kind := range kinds {
		lines = append(lines, lipgloss.JoinHorizontal(lipgloss.Top,
			m.renderer.NewStyle().Width(innerWidth-4).Render(kind),
			muted.Width(4).Align(lipgloss.Right).Render(fmt.Sprint(resources[kind]))))
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
