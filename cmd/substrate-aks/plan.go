package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/charmbracelet/x/term"

	"github.com/Azure/substrate-aks/internal/installer"
)

func writePlan(out io.Writer, subscription string, changes []installer.ResourceChange) error {
	renderer := lipgloss.NewRenderer(out)
	renderer.SetColorProfile(colorProfile(out))
	width := 0
	if file, ok := out.(interface{ Fd() uintptr }); ok && term.IsTerminal(file.Fd()) {
		if columns, _, err := term.GetSize(file.Fd()); err == nil {
			width = columns
		}
	}
	_, err := io.WriteString(out, renderPlan(renderer, subscription, changes, width))
	return err
}

func renderPlan(renderer *lipgloss.Renderer, subscription string, changes []installer.ResourceChange, width int) string {
	var rows [][]string
	var colors []lipgloss.Color
	for _, change := range changes {
		if change.ChangeType == "NoChange" {
			continue
		}
		label, color := planChange(change.ChangeType)
		rows = append(rows, []string{label, relativeResourceID(subscription, change.ResourceID)})
		colors = append(colors, color)
	}
	var body string
	if len(rows) == 0 {
		body = "No resource changes."
	} else {
		t := table.New().
			Headers("Change", "Resource ID").
			Rows(rows...).
			Border(lipgloss.ASCIIBorder()).
			BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).
			StyleFunc(func(row, column int) lipgloss.Style {
				style := renderer.NewStyle().Padding(0, 1)
				if row == table.HeaderRow {
					return style.Bold(true)
				}
				if column == 0 {
					return style.Bold(true).Foreground(colors[row])
				}
				return style
			})
		if width > 0 {
			t.Width(width)
		}
		body = t.String()
	}
	heading := renderer.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).Render("Planned resource changes")
	return fmt.Sprintf("\n%s\nScope: /subscriptions/%s (relative IDs)\n\n%s\n\nPreview only; nothing deployed.\nProperty-level details are in the diagnostic log (or use --verbose).\n\n",
		heading, subscription, body)
}

func planChange(changeType string) (string, lipgloss.Color) {
	switch changeType {
	case "Create":
		return "+ Create", lipgloss.Color("2")
	case "Delete":
		return "- Delete", lipgloss.Color("1")
	case "Modify":
		return "~ Modify", lipgloss.Color("3")
	case "Deploy":
		return "> Deploy", lipgloss.Color("6")
	default:
		return "? " + changeType, lipgloss.Color("3")
	}
}

func relativeResourceID(subscription, id string) string {
	parts := strings.SplitN(id, "/", 4)
	if len(parts) == 4 && parts[0] == "" &&
		strings.EqualFold(parts[1], "subscriptions") && strings.EqualFold(parts[2], subscription) {
		return "/" + parts[3]
	}
	return id
}
