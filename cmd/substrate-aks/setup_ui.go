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
	session  *setupSession
	review   *[]setupReviewEntry
}

const setupAccent = "#b48cf2"

func setupBrand(renderer *lipgloss.Renderer) string {
	const slab = "/___________/|\n|___________|/"
	purple := renderer.NewStyle().Foreground(lipgloss.Color(setupAccent))
	layers := purple.Render(" ____________\n" + slab + "\n" + slab + "\n" + slab)
	mark := renderer.NewStyle().PaddingRight(5).Render(layers)
	label := "\n" + purple.Bold(true).Render("s u b s t r a t e") + "\n\n" + purple.Render("on AKS") +
		"\n\n" + purple.Width(24).Render("An AKS packaging layer for Agent Substrate.")
	return lipgloss.JoinHorizontal(lipgloss.Top, mark, label)
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
	if u.session != nil {
		return
	}
	const tagline = "An AKS packaging layer for Agent Substrate."
	title := u.renderer.NewStyle().Bold(true)
	if decorative && u.width >= 64 {
		banner := setupBrand(u.renderer)
		fmt.Fprintln(u.out, "\n"+u.renderer.NewStyle().PaddingLeft(2).Render(banner)+"\n")
	} else {
		fmt.Fprintln(u.out, "\n"+title.Width(u.width).Render("Substrate on AKS"))
		u.note(tagline)
		fmt.Fprintln(u.out)
	}
	u.note("Configure Azure resources, review deployment settings, and install Substrate.")
}

func (u *setupUI) section(title string) {
	if u.review != nil {
		*u.review = append(*u.review, setupReviewEntry{kind: "heading", text: title})
		return
	}
	if u.session != nil {
		u.session.send(setupSectionMsg(title))
		return
	}
	fmt.Fprintln(u.out, "\n"+u.renderer.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).Render(title))
}

func (u *setupUI) checklist(title string, items ...string) {
	if u.session != nil {
		u.session.send(setupChecklistMsg{title, items})
	}
}

func (u *setupUI) check(name string, work func() error) error {
	if u.session == nil {
		u.section(name)
		return work()
	}
	u.session.send(setupCheckMsg{name: name})
	err := work()
	u.session.send(setupCheckMsg{name: name, done: true, err: err})
	return err
}

func (u *setupUI) complete(cluster string, withDemo bool) {
	if u.session != nil {
		return
	}
	brand := u.renderer.NewStyle().Bold(true).Foreground(lipgloss.Color(setupAccent))
	success := u.renderer.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
	content := brand.Render("Substrate on AKS") + "\n\n" + success.Render("DONE  Setup complete.") +
		"\nCluster: " + cluster
	if withDemo {
		content += "\nCounter microVM demo installed."
	}
	if u.width < 32 {
		fmt.Fprintln(u.out, "\n"+u.renderer.NewStyle().Width(u.width).Render(content))
		return
	}
	panel := u.renderer.NewStyle().Width(u.width-2).Padding(1, 2).
		Border(lipgloss.ASCIIBorder()).BorderForeground(lipgloss.Color(setupAccent))
	fmt.Fprintln(u.out, "\n"+panel.Render(content))
}

func (u *setupUI) command(command string) {
	style := u.renderer.NewStyle().Foreground(lipgloss.Color(setupAccent))
	for _, line := range strings.Split(command, "\n") {
		fmt.Fprintln(u.out, style.Render(line))
	}
}

func (u *setupUI) demo() {
	const post = "curl --fail-with-body -X POST \\\n  -H 'ate-target-actor: ate-demo-counter-microvm/my-counter' \\\n  http://localhost:8000/"
	u.section("# Try the counter demo:")
	u.section("# 1. Create a counter actor")
	u.command("kubectl ate create actor my-counter \\\n  -a ate-demo-counter-microvm --template counter-microvm")
	u.section("# 2. In another terminal, with the same KUBECONFIG")
	u.command("kubectl -n ate-system port-forward svc/atenet-router 8000:80")
	u.section("# 3. Increment the counters (repeat to see them increase)")
	u.command(post)
	u.section("# 4. Save the actor state and suspend synchronously")
	u.command("kubectl ate suspend actor my-counter \\\n  -a ate-demo-counter-microvm --mode=sync")
	u.section("# 5. Activate again; the counters should continue")
	u.command(post)
}

func (u *setupUI) note(text string) {
	if u.review != nil {
		*u.review = append(*u.review, setupReviewEntry{kind: "note", text: text})
		return
	}
	if u.session != nil {
		u.session.send(setupNoteMsg{text: text})
		return
	}
	fmt.Fprintln(u.out, u.renderer.NewStyle().Foreground(lipgloss.Color("8")).Width(u.width).Render(text))
}

func (u *setupUI) warning(text string) {
	if u.review != nil {
		*u.review = append(*u.review, setupReviewEntry{kind: "warning", text: text})
		return
	}
	if u.session != nil {
		u.session.send(setupNoteMsg{text: text, warning: true})
		return
	}
	fmt.Fprintln(u.out, u.renderer.NewStyle().Foreground(lipgloss.Color("3")).Width(u.width).Render("WARNING: "+text))
}

func (u *setupUI) row(label, value string) {
	if u.review != nil {
		*u.review = append(*u.review, setupReviewEntry{kind: "row", label: label, text: value})
		return
	}
	if u.session != nil {
		u.session.send(setupNoteMsg{text: label + ": " + value})
		return
	}
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
	if u.session != nil {
		var entries []setupReviewEntry
		plain := newSetupUI(io.Discard)
		plain.review = &entries
		plain.summary(c, defaults, path, created, engine)
		u.session.send(setupReviewMsg(entries))
		return
	}
	u.section("Installation summary")
	if created {
		message := "Configuration saved: " + path
		if u.review != nil {
			*u.review = append(*u.review, setupReviewEntry{kind: "success", text: message})
		} else {
			fmt.Fprintln(u.out, u.renderer.NewStyle().Foreground(lipgloss.Color("2")).Width(u.width).Render(message))
		}
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

func (u *setupUI) step(text string) {
	if u.session != nil {
		u.session.send(setupStepMsg(text))
		return
	}
	u.note(text)
}
