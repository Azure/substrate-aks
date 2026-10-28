package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/Azure/substrate-aks/internal/installer"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
)

type setupUI struct {
	out      io.Writer
	renderer *lipgloss.Renderer
	width    int
}

func newSetupUI(out io.Writer) *setupUI {
	width := 80
	if file, ok := out.(interface{ Fd() uintptr }); ok && term.IsTerminal(file.Fd()) {
		if columns, _, err := term.GetSize(file.Fd()); err == nil && columns > 0 {
			width = min(columns, 88)
		}
	}
	return &setupUI{out: out, renderer: newConsole(out).renderer, width: width}
}

func (u *setupUI) welcome(decorative bool) {
	const tagline = "An AKS packaging layer for Agent Substrate."
	title := u.renderer.NewStyle().Bold(true)
	if decorative && u.width >= 64 {
		const slab = "/___________/|\n|___________|/"
		purple := u.renderer.NewStyle().Foreground(lipgloss.Color("#b48cf2"))
		layers := purple.Render(" ____________\n" + slab + "\n" + slab + "\n" + slab)
		mark := u.renderer.NewStyle().PaddingRight(5).Render(layers)
		label := "\n" + purple.Bold(true).Render("s u b s t r a t e") + "\n\n" + purple.Render("on AKS") +
			"\n\n" + purple.Width(24).Render(tagline)
		banner := lipgloss.JoinHorizontal(lipgloss.Top, mark, label)
		fmt.Fprintln(u.out, "\n"+u.renderer.NewStyle().PaddingLeft(2).Render(banner)+"\n")
	} else {
		fmt.Fprintln(u.out, "\n"+title.Width(u.width).Render("Substrate on AKS"))
		u.note(tagline)
		fmt.Fprintln(u.out)
	}
	u.note("Configure Azure resources, review deployment settings, and install Substrate.")
}

func (u *setupUI) section(title string) {
	fmt.Fprintln(u.out, "\n"+u.renderer.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).Render(title))
}

func (u *setupUI) note(text string) {
	fmt.Fprintln(u.out, u.renderer.NewStyle().Foreground(lipgloss.Color("8")).Width(u.width).Render(text))
}

func (u *setupUI) warning(text string) {
	fmt.Fprintln(u.out, u.renderer.NewStyle().Foreground(lipgloss.Color("3")).Width(u.width).Render("WARNING: "+text))
}

func (u *setupUI) row(label, value string) {
	left := u.renderer.NewStyle().Foreground(lipgloss.Color("8"))
	right := u.renderer.NewStyle()
	if u.width < 64 {
		fmt.Fprintln(u.out, left.Render(label))
		fmt.Fprintln(u.out, right.PaddingLeft(2).Width(u.width).Render(value))
		return
	}
	fmt.Fprintln(u.out, lipgloss.JoinHorizontal(lipgloss.Top,
		left.Width(20).Render(label), right.Width(u.width-20).Render(value)))
}

func (u *setupUI) identity(label, name, id string) {
	u.row(label, formatAzureID(name, id))
}

func (u *setupUI) summary(c installer.Config, defaults azureDefaults, path string, created bool, engine string) {
	u.section("Installation summary")
	if created {
		fmt.Fprintln(u.out, u.renderer.NewStyle().Foreground(lipgloss.Color("2")).Width(u.width).Render("Configuration saved: "+path))
		u.note("No Azure resources have been created.")
	} else {
		u.note("Using saved configuration: " + path)
	}
	fmt.Fprintln(u.out)
	u.row("Resource group", c.Azure.ResourceGroup)
	u.row("Cluster", c.AKS.ClusterName)
	u.row("Region", c.Azure.Location)
	u.identity("Subscription", defaults.subscriptionName, c.Azure.SubscriptionID)
	u.identity("Azure identity", defaults.publisherName, c.Azure.AssetPublisherObjectID)
	if operator := c.AKS.Operator; operator != nil {
		u.row("Operator auth", "Managed Entra / Azure RBAC; local accounts disabled")
		u.row("AKS operator", operator.ObjectID+" ("+operator.PrincipalType+")")
		u.note("Cluster-scoped AKS RBAC Cluster Admin, including credential-fetch access. Uses your current az login through kubelogin.")
	} else {
		u.row("Operator auth", "Legacy configuration (unchanged)")
		u.note("This config predates managed Entra defaults. Setup does not migrate existing cluster authentication.")
	}
	u.row("AKS version", c.AKS.KubernetesVersion)
	u.row("Nodes", fmt.Sprintf("1 system + %d microVM", c.AKS.WorkerPool.Count))
	u.row("Node size", c.AKS.WorkerPool.VMSize)
	u.row("Image builder", engine)
	if c.AKS.AllowUnrestrictedAPI {
		u.row("API access", "Unrestricted (internet-reachable)")
		u.warning(unrestrictedAPIWarning)
	} else {
		u.row("API access", strings.Join(c.AKS.APIAuthorizedCIDRs, ", "))
	}
	if c.Azure.SnapshotReaderObjectID == "" {
		u.row("Snapshot inspection", "No new operator access")
		u.note("Runtime identities receive access. Existing operator grants are not revoked.")
	} else {
		u.row("Snapshot inspection", "Read-only, snapshots container only")
		u.row("Reader object ID", c.Azure.SnapshotReaderObjectID)
	}
	u.section("Before you continue")
	u.warning("This creates billable ACR, AKS, and storage resources.")
	u.note("Failed runs retain resources for retry. Teardown deletes all installation data.")
}
