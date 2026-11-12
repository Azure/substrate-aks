package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Azure/substrate-aks/internal/installer"
)

const usage = `substrate-aks <command> [NAME] [flags]

An AKS packaging layer for Agent Substrate.
Azure infrastructure, declarative deployment configuration, and a guided installer.

Installations are saved by name in a per-user state directory:
~/.local/state/substrate-aks on Linux ($XDG_STATE_HOME), ~/Library/Application
Support/substrate-aks on macOS, or SUBSTRATE_AKS_HOME if set.
NAME may be omitted when exactly one installation exists, except for init,
setup and destroy. Without NAME, init and setup --yes create substrate-dev,
interactive setup asks for a name, and destroy refuses to run.

Commands:
  setup      Guide provisioning, installation, and client setup; resumes NAME if it exists
  list       List saved installations
  init       Save a new installation; creates no Azure resources
  doctor     Check local tools; with NAME, check that installation; --azure adds read-only cloud checks
  plan       Run Bicep subscription what-if; requires Azure access
  provision  Create/reconcile billable resources owned by this installation
  images     Build/push images to owned ACR and assemble microVM assets
  check      Check cluster version, nodes and required certificate APIs
  prepare    Fetch exact source and adapt manifests in a managed checkout
  install    Install Substrate and microVM assets (requires compatible images)
  demo       Deploy upstream's counter-microvm demo
  client     Build the matching kubectl-ate and print shell configuration
  status     Show Substrate pods and persistent volume claims
  destroy    Delete the owned resource group and ALL installation data

Common flags:
  --name NAME                            Same as the NAME argument
  --verbose                              Show subprocess commands and diagnostics
  --registry-only                        (plan/provision only; ACR bootstrap)
  --component ate-system|atelet|apiserver  (install only; default ate-system)
  --confirm-delete RESOURCE_GROUP        (destroy only)
  --check                                (images only; verify saved publication)
  --reset-config                         (images only; clear images after teardown)
  --container-engine auto|docker|podman   (setup/images; setup offers a choice if both work)
  --azure                                (doctor only; read-only cloud checks, requires NAME)

init flags:
  --subscription UUID                    Default: active Azure CLI subscription
  --identity-object-id UUID               Default: signed-in Azure CLI identity
  --identity-principal-type TYPE          Explicit publisher type: User, ServicePrincipal, or Group
  --operator-object-id UUID               Default: signed-in identity, independent of upload identity
  --operator-principal-type TYPE          User (explicit ID default), ServicePrincipal, or Group
  --api-cidr YOUR_IP/32                   Default: detected public IPv4 /32
  --allow-unrestricted-api               Opt out of the AKS API IP allowlist
  --snapshot-reader-object-id UUID        Optional read-only snapshot inspection
  --snapshot-reader-principal-type TYPE   Explicit reader type: User, ServicePrincipal, or Group
  --name substrate-dev --location eastus2

setup flags:
  Same configuration flags as init when creating a new installation; without
  NAME, interactive setup asks for one and --yes uses substrate-dev
  --yes                                  Accept billable deployment and select the kube-context without prompts
  --with-demo                            Also deploy the counter microVM demo
  --accessible                           Use screen-reader-friendly prompts
  --context-file PATH                    Destination for the final context handoff
  --skip-context-handoff                 Skip importing/selecting a context; print private kubeconfig exports

client flags:
  --use-context                          Explicitly import/select this installation in your kubeconfig
  --context-file PATH                    Destination; defaults to a single KUBECONFIG file or ~/.kube/config
`

func main() {
	ctx, cancel := interruptContext(context.Background())
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		reportCommandError(os.Stderr, err)
		os.Exit(1)
	}
}

func reportCommandError(out io.Writer, err error) {
	console := newConsole(out)
	if errors.Is(err, context.Canceled) {
		console.logger.Print("Stopped by user.")
		if err.Error() != context.Canceled.Error() {
			console.logger.Print(err)
		}
		return
	}
	console.logger.Error(err)
}

func run(ctx context.Context, args []string, out, stderr io.Writer) error {
	return runCommand(ctx, args, out, stderr, nil)
}

func runCommand(ctx context.Context, args []string, out, stderr io.Writer, clientResult *installer.ClientSetup) (resultErr error) {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(out, usage)
		return err
	}
	command := args[0]
	switch command {
	case "init", "setup", "list", "doctor", "plan", "provision", "images", "check", "prepare", "install", "demo", "client", "status", "destroy":
	default:
		return fmt.Errorf("unknown command %q; use --help", command)
	}
	home, err := installer.Home()
	if err != nil {
		return err
	}
	if command == "list" {
		if len(args) > 1 {
			return fmt.Errorf("unexpected arguments: %v", args[1:])
		}
		return listInstallations(out, home)
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	// The installation name may come before or after the flags.
	flagArgs, positional := args[1:], ""
	if len(flagArgs) > 0 && !strings.HasPrefix(flagArgs[0], "-") {
		positional, flagArgs = flagArgs[0], flagArgs[1:]
	}
	verbose := fs.Bool("verbose", false, "show subprocess commands and diagnostics")
	var confirmation, component string
	var subscription, publisher, reader, cidr, name, location string
	var publisherType, readerType string
	var operatorID, operatorType string
	var contextFile string
	var useContext bool
	var yes, withDemo, unrestrictedAPI, accessible, skipContextHandoff bool
	var registryOnly, checkImages, resetImages bool
	var azureChecks bool
	var containerEngine string
	if command == "setup" || command == "images" || command == "doctor" {
		fs.StringVar(&containerEngine, "container-engine", "", "image builder: auto (Docker/buildx then Podman), docker or podman")
	}
	if command == "doctor" {
		fs.BoolVar(&azureChecks, "azure", false, "check Azure deployment permissions, AKS versions, SKU availability and CPU quota (requires NAME)")
	}
	if command == "plan" || command == "provision" {
		fs.BoolVar(&registryOnly, "registry-only", false, "bootstrap only the owned resource group and private ACR")
	}
	if command == "destroy" {
		fs.StringVar(&confirmation, "confirm-delete", "", "resource group name, acknowledging complete data deletion")
	}
	if command == "install" {
		fs.StringVar(&component, "component", "ate-system", "ate-system, atelet or apiserver")
	}
	if command == "client" || command == "setup" {
		fs.StringVar(&contextFile, "context-file", "", "destination for the Kubernetes context handoff")
	}
	if command == "client" {
		fs.BoolVar(&useContext, "use-context", false, "import and select this installation in the destination kubeconfig")
	}
	if command == "images" {
		fs.BoolVar(&checkImages, "check", false, "verify saved images without publishing or changing configuration")
		fs.BoolVar(&resetImages, "reset-config", false, "clear saved image references only after the installation resource group is absent")
	}
	if command == "init" || command == "setup" {
		fs.StringVar(&subscription, "subscription", "", "Azure subscription UUID (default: active Azure CLI subscription)")
		fs.StringVar(&publisher, "identity-object-id", "", "Azure identity Entra object UUID used for image/asset uploads (default: signed-in identity)")
		fs.StringVar(&publisher, "asset-publisher-object-id", "", "compatibility alias for --identity-object-id")
		fs.StringVar(&publisherType, "identity-principal-type", "", "publisher type: User, ServicePrincipal, or Group (otherwise discovered or inferred)")
		fs.StringVar(&operatorID, "operator-object-id", "", "Entra object UUID granted cluster-scoped AKS RBAC Cluster Admin (default: signed-in identity)")
		fs.StringVar(&operatorType, "operator-principal-type", "", "User, ServicePrincipal, or Group (explicit object ID defaults to User)")
		fs.StringVar(&reader, "snapshot-reader-object-id", "", "optional Entra object UUID granted read-only snapshot access")
		fs.StringVar(&readerType, "snapshot-reader-principal-type", "", "reader type: User, ServicePrincipal, or Group (otherwise inferred from a matching identity)")
		fs.StringVar(&cidr, "api-cidr", "", "AKS API IPv4 CIDR (default: detected public IPv4 /32)")
		fs.BoolVar(&unrestrictedAPI, "allow-unrestricted-api", false, "allow internet-wide AKS API access instead of an IP allowlist")
		fs.StringVar(&location, "location", "eastus2", "Azure region")
	}
	if command == "setup" {
		fs.BoolVar(&yes, "yes", false, "accept billable deployment and select the kube-context without prompts")
		fs.BoolVar(&withDemo, "with-demo", false, "also deploy the counter microVM demo")
		fs.BoolVar(&accessible, "accessible", false, "use screen-reader-friendly prompts instead of interactive forms")
		fs.BoolVar(&skipContextHandoff, "skip-context-handoff", false, "skip importing/selecting a context and print private kubeconfig exports")
	}
	fs.StringVar(&name, "name", "", "installation name; for new installations, also the dedicated resource group and cluster name")
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if positional == "" && fs.NArg() > 0 {
		positional = fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return err
		}
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if positional != "" {
		if name != "" && name != positional {
			return fmt.Errorf("installation name given twice: %q and --name %q", positional, name)
		}
		name = positional
	}
	if name != "" {
		if err := installer.ValidateInstallationName(name); err != nil {
			return fmt.Errorf("installation name %q %w", name, err)
		}
	}
	if err := installer.ValidateContainerEngine(containerEngine); err != nil {
		return err
	}
	if unrestrictedAPI && cidr != "" {
		return errors.New("--api-cidr and --allow-unrestricted-api are mutually exclusive")
	}
	if operatorType != "" && operatorID == "" {
		return errors.New("--operator-principal-type requires --operator-object-id")
	}
	if publisherType != "" && publisher == "" {
		return errors.New("--identity-principal-type requires --identity-object-id")
	}
	if readerType != "" && reader == "" {
		return errors.New("--snapshot-reader-principal-type requires --snapshot-reader-object-id")
	}
	if command == "client" && contextFile != "" && !useContext {
		return errors.New("client --context-file requires --use-context; without it client only prints private shell configuration")
	}
	if skipContextHandoff && contextFile != "" {
		return errors.New("--skip-context-handoff and --context-file are mutually exclusive")
	}
	if checkImages && resetImages {
		return errors.New("--check and --reset-config are mutually exclusive")
	}
	if command == "init" {
		if name == "" {
			name = "substrate-dev"
		}
		if _, err := os.Stat(installer.RecordPath(home, name)); err == nil {
			return fmt.Errorf("installation %q already exists; init never overwrites an existing installation: %w", name, os.ErrExist)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		fmt.Fprintln(stderr, "Discovering Azure subscription and installation identities.")
		defaults, err := discoverInstallationDefaults(ctx, subscription, publisher, operatorID, operatorType, installer.ExecRunner{})
		if err != nil {
			return err
		}
		regions, err := readAzureRegions(ctx, defaults.subscription, installer.ExecRunner{})
		if err != nil {
			return err
		}
		location, err = validateAzureRegion(location, regions)
		if err != nil {
			return err
		}
		cidr, err = resolveAPICIDR(ctx, cidr, unrestrictedAPI, nil)
		if err != nil {
			return err
		}
		printAzureDefaults(out, defaults)
		subscription, publisher = defaults.subscription, defaults.publisher
		if publisherType == "" && strings.EqualFold(publisher, defaults.signedInObjectID) {
			publisherType = defaults.signedInType
		}
		return initialize(home, subscription, publisher, reader, cidr, name, location, unrestrictedAPI, &defaults.operator, publisherType, readerType, out)
	}
	if command == "setup" {
		var client installer.ClientSetup
		var configFlags bool
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "subscription", "identity-object-id", "asset-publisher-object-id", "identity-principal-type", "operator-object-id", "operator-principal-type", "snapshot-reader-object-id", "snapshot-reader-principal-type", "api-cidr", "allow-unrestricted-api", "location":
				configFlags = true
			}
		})
		// Interactive setup asks for a name instead of prefilling the default.
		if name == "" && yes {
			name = "substrate-dev"
		}
		return setup(ctx, setupOptions{
			home: home, subscription: subscription, publisher: publisher, reader: reader,
			cidr: cidr, name: name, location: location, yes: yes, withDemo: withDemo,
			verbose: *verbose, configFlags: configFlags, unrestrictedAPI: unrestrictedAPI, accessible: accessible,
			containerEngine: containerEngine,
			operatorID:      operatorID, operatorType: operatorType,
			publisherType: publisherType, readerType: readerType,
			contextFile: contextFile, skipContextHandoff: skipContextHandoff,
			client: &client,
		}, os.Stdin, out, stderr, func(ctx context.Context, args []string, out, stderr io.Writer) error {
			return runCommand(ctx, args, out, stderr, &client)
		})
	}
	if command == "doctor" {
		if name == "" {
			if azureChecks {
				return errors.New("doctor --azure requires an installation NAME")
			}
			if containerEngine != "" {
				return errors.New("doctor --container-engine requires an installation NAME for an installation-specific builder probe")
			}
			p := newProgress(stderr, io.Discard, "Local prerequisites", false)
			defer p.stop()
			a := installer.App{Runner: installer.ExecRunner{}, Progress: p.step}
			if err := a.Doctor(ctx); err != nil {
				return err
			}
			p.complete("Local prerequisites complete.")
			return nil
		}
	}
	if command == "destroy" && name == "" {
		return errors.New("destroy requires the installation NAME")
	}
	name, err = installer.ResolveInstallation(home, name)
	if err != nil {
		return err
	}
	configPath := installer.RecordPath(home, name)
	c, err := installer.LoadInstallation(home, name)
	if err != nil {
		return err
	}
	lock, err := installer.Lock(c)
	if err != nil {
		return err
	}
	defer lock.Close()
	// Another command may have destroyed or updated the installation between
	// loading it and taking the lock.
	if c, err = installer.LoadInstallation(home, name); err != nil {
		return err
	}
	log, err := installer.NewLog(c)
	if err != nil {
		return fmt.Errorf("creating command log: %w", err)
	}
	defer log.Close()
	titles := map[string]string{
		"doctor": "Local prerequisites", "plan": "Infrastructure preview",
		"provision": "Infrastructure provisioning", "images": "Image and runtime asset preparation",
		"check": "Cluster checks", "prepare": "Source preparation",
		"install": "Substrate installation", "demo": "Counter demo deployment",
		"client": "Client setup", "status": "Installation status", "destroy": "Installation teardown",
	}
	title := titles[command]
	if command == "doctor" && azureChecks {
		title = "Azure deployment prerequisites"
	}
	if checkImages {
		title = "Saved image verification"
	} else if resetImages {
		title = "Saved image reset"
	}
	p := commandProgress(ctx, stderr, log, title, !*verbose && command != "status")
	session := setupSessionFrom(ctx)
	if session != nil {
		session.send(setupLogMsg(log.Name()))
		out = log
	}
	fmt.Fprintf(log, "[command] %s\n[installation] %s\n[target] %s / %s\n", command, configPath, c.Azure.ResourceGroup, c.Azure.Location)
	p.heading(title)
	if command != "doctor" {
		p.info("Target: %s / %s", c.Azure.ResourceGroup, c.Azure.Location)
	}
	if c.AKS.AllowUnrestrictedAPI && (command == "plan" || command == "provision") {
		p.warn(unrestrictedAPIWarning)
	}
	defer func() {
		p.stop()
		if resultErr != nil {
			fmt.Fprintf(log, "\n[error] %v\n", resultErr)
			state := "failed"
			if errors.Is(resultErr, context.Canceled) {
				state = "interrupted"
			}
			resultErr = fmt.Errorf("%s %s.\nLast step: %s\n%w\n\nDiagnostic log: %s", title, state, p.stage, resultErr, log.Name())
			return
		}
		p.complete("%s complete (%s).", title, time.Since(p.started).Round(time.Second))
		fmt.Fprintf(log, "\n[complete] %s\n", title)
	}()
	runner := installer.ExecRunner{Log: log, Stdout: out, Stderr: p}
	if session != nil {
		runner.Stdout, runner.Stderr = nil, nil
	}
	if *verbose {
		if session == nil {
			runner.Verbose = p
		}
		p.info("Diagnostic log: %s", log.Name())
	}
	a := installer.App{Config: c, Runner: runner, ContainerEngine: containerEngine, Progress: func(format string, args ...any) {
		message := fmt.Sprintf(format, args...)
		if strings.HasPrefix(message, "WARNING: ") {
			p.warn(strings.TrimPrefix(message, "WARNING: "))
		} else {
			p.step("%s", message)
		}
	}, Preview: func(changes []installer.ResourceChange) error {
		p.pause()
		if session != nil {
			return session.preview(c.Azure.SubscriptionID, changes)
		}
		return writePlan(out, c.Azure.SubscriptionID, changes)
	}}
	if session != nil && checkImages {
		a.ImageCheckProgress = func(name string, done bool, err error) {
			session.send(setupCheckMsg{name: name, done: done, err: err})
		}
	}
	switch command {
	case "doctor":
		if err := a.Doctor(ctx); err != nil {
			return err
		}
		if err := a.RuntimePrerequisites(ctx); err != nil {
			return err
		}
		if azureChecks {
			return a.CloudPreflight(ctx)
		}
		return nil
	case "plan":
		if registryOnly {
			return a.ProvisionRegistry(ctx, true)
		}
		return a.Provision(ctx, true)
	case "provision":
		p.warn("This creates billable resources. Failed or interrupted runs retain resources for retry.")
		if registryOnly {
			return a.ProvisionRegistry(ctx, false)
		}
		return a.Provision(ctx, false)
	case "images":
		if checkImages {
			return a.CheckImages(ctx)
		}
		if resetImages {
			if err := a.ResetImageConfig(ctx, configPath); err != nil {
				return err
			}
			p.pause()
			fmt.Fprintf(out, "Cleared saved image references for %s after confirming resource-group absence. Runtime asset settings are unchanged.\nNext: %s\n", name, cliCommand("setup", name))
			return nil
		}
		p.info("Builds and downloads can take several minutes. Failed runs may leave partial images in ACR.")
		result, err := a.BuildImages(ctx)
		if err != nil {
			return err
		}
		p.step("Saving image references and runtime asset paths")
		if err := installer.SaveBuildResult(configPath, c, result); err != nil {
			return err
		}
		p.pause()
		fmt.Fprintf(out, "Saved ACR image references and runtime assets for %s.\n", name)
		return nil
	case "check":
		return a.Check(ctx)
	case "prepare":
		root, err := a.Prepare(ctx)
		if err == nil {
			p.pause()
			fmt.Fprintln(out, "Managed source:", root)
		}
		return err
	case "install":
		return a.Install(ctx, component)
	case "demo":
		return a.Demo(ctx)
	case "client":
		client, err := a.SetupClient(ctx)
		if err != nil {
			return err
		}
		p.pause()
		if clientResult != nil {
			*clientResult = client
			return nil
		}
		if useContext {
			selection, err := installer.PlanContextSwitch(contextFile)
			if err != nil {
				return err
			}
			if err := a.SelectContext(ctx, selection); err != nil {
				return err
			}
			client.Kubeconfig = selection.Path
		}
		return printClientSetup(out, client)
	case "status":
		return a.Status(ctx)
	case "destroy":
		if err := a.Destroy(ctx, confirmation); err != nil {
			return err
		}
		p.step("Removing local installation state")
		return installer.RemoveInstallation(c)
	}
	return nil
}

func initialize(home, subscription, publisher, reader, cidr, name, location string, unrestrictedAPI bool, operator *installer.OperatorConfig, publisherType, readerType string, out io.Writer) error {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	c := installer.DefaultConfig()
	c.Installation.ID = fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
	c.Installation.Name = name
	c.Azure = installer.AzureConfig{
		SubscriptionID: subscription, AssetPublisherObjectID: publisher,
		SnapshotReaderObjectID:      reader,
		AssetPublisherPrincipalType: publisherType, SnapshotReaderPrincipalType: readerType,
		Location: location, ResourceGroup: name,
	}
	c.AKS.ClusterName = name
	c.AKS.Operator = operator
	c.AKS.AllowUnrestrictedAPI = unrestrictedAPI
	c.AKS.APIAuthorizedCIDRs = []string{}
	if cidr != "" {
		c.AKS.APIAuthorizedCIDRs = []string{cidr}
	}
	path, err := installer.CreateInstallation(home, c)
	if err != nil {
		return err
	}
	printAPIAccess(out, c.AKS)
	fmt.Fprintf(out, "Saved installation %s in %s. No Azure resources were created.\nNext: %s\n", name, path, cliCommand("setup", name))
	return nil
}

func listInstallations(out io.Writer, home string) error {
	names, err := installer.ListInstallations(home)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		_, err := fmt.Fprintf(out, "No installations in %s. Run setup to create one.\n", home)
		return err
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tRESOURCE GROUP\tREGION\tIMAGES")
	for _, name := range names {
		c, err := installer.LoadInstallation(home, name)
		if err != nil {
			fmt.Fprintf(w, "%s\t(unreadable: %v)\t\t\n", name, err)
			continue
		}
		images := "not published"
		if c.Substrate.Images != (installer.ImagesConfig{}) {
			images = "published"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, c.Azure.ResourceGroup, c.Azure.Location, images)
	}
	return w.Flush()
}
