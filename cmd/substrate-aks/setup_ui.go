package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Azure/substrate-aks/internal/installer"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
	"github.com/muesli/termenv"
)

type setupUI struct {
	out      io.Writer
	renderer *lipgloss.Renderer
	width    int
	session  *setupSession
	review   *[]setupReviewEntry
	// log records workspace checks and steps before stage commands own logging.
	log io.Writer
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

func newSetupReceiptUI(ctx context.Context, out io.Writer) *setupUI {
	if session := setupSessionFrom(ctx); session != nil && session.receipt != nil {
		ui := *session.receipt
		ui.out = out
		return &ui
	}
	return newSetupUI(out)
}

func (u *setupUI) welcome(decorative bool) {
	if u.session != nil {
		return
	}
	const tagline = "An AKS packaging layer for Agent Substrate."
	title := u.renderer.NewStyle().Bold(true)
	if banner := setupBrand(u.renderer, 0); decorative && lipgloss.Width(banner)+2 <= u.width {
		fmt.Fprintln(u.out, "\n"+u.renderer.NewStyle().PaddingLeft(2).Render(banner)+"\n")
	} else {
		fmt.Fprintln(u.out, "\n"+title.Width(u.width).Render("Agent Substrate on AKS"))
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
	// Style lines separately so multi-line headings are not padded to a common width.
	style := u.renderer.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	lines := strings.Split(title, "\n")
	for i, line := range lines {
		lines[i] = style.Render(line)
	}
	fmt.Fprintln(u.out, "\n"+strings.Join(lines, "\n"))
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
	u.logf("\n[step] %s\n", name)
	err := work()
	if err != nil {
		u.logf("\n[error] %s: %v\n", name, err)
	}
	u.session.send(setupCheckMsg{name: name, done: true, err: err})
	return err
}

func (u *setupUI) logf(format string, args ...any) {
	if u.log != nil {
		fmt.Fprintf(u.log, format, args...)
	}
}

func (u *setupUI) checkGroup(names []string, work func() error) error {
	u.session.send(setupCheckGroupMsg{names: names})
	u.session.send(setupCheckMsg{name: names[0]})
	err := work()
	u.session.send(setupCheckGroupMsg{names: names, done: true, err: err})
	return err
}

func (u *setupUI) complete(cluster string, withDemo bool) {
	if u.session != nil {
		return
	}
	plain := u.renderer.ColorProfile() == termenv.Ascii
	st := newSetupStyles(u.renderer)
	brand, border := setupBadge(u.renderer), lipgloss.RoundedBorder()
	if plain {
		brand, border = "Agent Substrate on AKS", lipgloss.ASCIIBorder()
	}
	success := u.renderer.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
	row := func(label, value string) string {
		// Long identifiers move below their label instead of breaking mid-word.
		if lipgloss.Width(label+value) > u.width-6 {
			return "\n" + st.muted.Render(strings.TrimSpace(label)) + "\n" + value
		}
		return "\n" + st.muted.Render(label) + value
	}
	content := brand + "\n\n" + success.Render("DONE  Setup complete.") + row("Cluster  ", cluster)
	if withDemo {
		content += row("Demo     ", "Counter microVM demo installed.")
	}
	if u.width < 32 {
		fmt.Fprintln(u.out, "\n"+u.renderer.NewStyle().Width(u.width).Render(content))
		return
	}
	panel := u.renderer.NewStyle().Width(u.width-2).Padding(1, 2).
		Border(border).BorderForeground(lipgloss.Color(setupAccent))
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
	u.section("# Try the counter demo:\n# 1. Create a counter actor")
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

// identity shows a display name with its object ID; the review screen
// puts the ID on its own muted line so names stay scannable.
func (u *setupUI) identity(label, name, id string) {
	u.principal(label, name, id, "", "")
}

// principal shows an Entra principal by name when known, its object ID and
// type as supporting detail, and an optional explanation.
func (u *setupUI) principal(label, name, id, kind, explanation string) {
	if kind != "" {
		id += " · " + kind
	}
	if u.review == nil {
		u.row(label, formatAzureID(name, id))
		if explanation != "" {
			u.note(explanation)
		}
		return
	}
	value, details := id, []string{}
	if name != "" {
		value, details = name, append(details, id)
	}
	if explanation != "" {
		details = append(details, explanation)
	}
	u.rowDetail(label, value, strings.Join(details, "\n"))
}

// group starts a titled block of summary rows.
func (u *setupUI) group(title string) {
	if u.review != nil {
		*u.review = append(*u.review, setupReviewEntry{kind: "group", text: title})
		return
	}
	u.section(title)
}

// rowDetail is a summary row with a short explanation under its value.
func (u *setupUI) rowDetail(label, value, detail string) {
	if u.review != nil {
		*u.review = append(*u.review, setupReviewEntry{kind: "row", label: label, text: value, detail: detail})
		return
	}
	u.row(label, value)
	u.note(detail)
}

// callout closes the summary with the warnings to read before approving.
func (u *setupUI) callout(title string, warnings []string, note string) {
	if u.review != nil {
		*u.review = append(*u.review, setupReviewEntry{kind: "callout", label: title, text: strings.Join(warnings, "\n"), detail: note})
		return
	}
	u.section(title)
	for _, warning := range warnings {
		u.warning(warning)
	}
	u.note(note)
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
	if u.review == nil {
		u.section("Installation summary")
	}
	if created {
		if u.review != nil {
			*u.review = append(*u.review, setupReviewEntry{kind: "success", text: "Configuration saved · nothing has been created in Azure yet", detail: path})
		} else {
			fmt.Fprintln(u.out, u.renderer.NewStyle().Foreground(lipgloss.Color("2")).Width(u.width).Render("Configuration saved: "+path))
			u.note("No Azure resources have been created.")
		}
	} else {
		u.note("Using saved configuration: " + path)
	}
	publisher := c.Azure.AssetPublisherObjectID
	same := func(id string) bool { return strings.EqualFold(id, publisher) }
	operator, reader := c.AKS.Operator, c.Azure.SnapshotReaderObjectID
	// The operator and snapshot reader default to the Azure identity; list
	// them as its roles instead of repeating the same principal.
	roles := []string{"publishes images and assets"}
	if operator != nil && same(operator.ObjectID) {
		roles = append(roles, "cluster admin")
	}
	if reader != "" && same(reader) {
		roles = append(roles, "snapshot reader")
	}

	warnings := []string{"This creates billable ACR, AKS, and storage resources."}
	u.group("Azure")
	u.identity("Subscription", defaults.subscriptionName, c.Azure.SubscriptionID)
	u.row("Resource group", c.Azure.ResourceGroup)
	u.row("Region", c.Azure.Location)
	u.principal("Azure identity", defaults.publisherName, publisher, c.Azure.AssetPublisherPrincipalType,
		"Roles: "+strings.Join(roles, ", "))

	u.group("Cluster")
	u.row("Cluster", c.AKS.ClusterName)
	u.row("AKS version", c.AKS.KubernetesVersion)
	u.row("Nodes", fmt.Sprintf("1 system + %d microVM", c.AKS.WorkerPool.Count))
	u.row("Node size", c.AKS.WorkerPool.VMSize)
	if c.AKS.AllowUnrestrictedAPI {
		u.row("API server access", "Anyone on the internet")
		warnings = append(warnings, unrestrictedAPIWarning)
	} else {
		u.row("API server access", "Only "+strings.Join(c.AKS.APIAuthorizedCIDRs, ", "))
	}
	if operator != nil {
		u.row("Sign-in", "Entra ID and Azure RBAC via kubelogin; no local accounts")
	} else {
		u.rowDetail("Sign-in", "Legacy configuration (unchanged)",
			"This config predates managed Entra defaults. Setup does not migrate existing cluster authentication.")
	}

	u.group("Substrate")
	u.row("Version", c.Substrate.Source.Describe(installer.DefaultRepository))
	switch {
	case c.Substrate.Images != (installer.ImagesConfig{}):
		u.row("Images", "Saved: "+c.Substrate.Images.Repository+":"+c.Substrate.Images.Tag)
	case c.EffectiveImageSource() == installer.ImageSourceBuild:
		u.rowDetail("Images", "Built from source into the installation's ACR",
			"Includes the counter demo and the Azure Blob plugin.")
	default:
		u.rowDetail("Images", "Release images imported into the installation's ACR",
			"From "+installer.DefaultReleaseImages+", checked against the pinned commit. The counter demo and Azure Blob plugin are built.")
	}
	u.row("Storage plugin", "Azure Blob, "+c.Substrate.PluginSource.Describe(installer.DefaultPluginRepository))
	u.row("Image builder", engine)

	access := operator != nil && !same(operator.ObjectID) || !same(reader)
	if access {
		u.group("Access")
	}
	if operator != nil && !same(operator.ObjectID) {
		u.principal("Cluster admin", "", operator.ObjectID, operator.PrincipalType, "")
	}
	if reader == "" {
		u.rowDetail("Snapshot inspection", "No new operator access",
			"Runtime identities receive access. Existing operator grants are not revoked.")
	} else if !same(reader) {
		u.principal("Snapshot reader", "", reader, "", "Read-only access to the snapshots container.")
	}
	u.callout("Before you continue", warnings, "Failed runs retain resources for retry. Teardown deletes all installation data.")
}

func (u *setupUI) step(text string) {
	if u.session != nil {
		u.session.send(setupStepMsg(text))
		u.logf("\n[step] %s\n", text)
		return
	}
	u.note(text)
}
