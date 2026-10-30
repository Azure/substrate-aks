package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Azure/substrate-aks/internal/installer"
)

const usage = `substrate-aks <command> [flags]

An AKS packaging layer for Agent Substrate.
Azure infrastructure, declarative deployment configuration, and a guided installer.

Commands:
  setup      Guide provisioning, installation, and client setup
  init       Write a new configuration; creates no Azure resources
  doctor     Check local tools; --azure adds read-only cloud deployment checks
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
  --config deploy.json
  --verbose                              Show subprocess commands and diagnostics
  --registry-only                        (plan/provision only; ACR bootstrap)
  --component ate-system|atelet|apiserver  (install only; default ate-system)
  --confirm-delete RESOURCE_GROUP        (destroy only)
  --check                                (images only; verify saved publication)
  --reset-config                         (images only; clear images after teardown)
  --container-engine auto|docker|podman   (setup/images; setup offers a choice if both work)
  --azure                                (doctor only; read-only cloud checks, requires --config)

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
  Same configuration flags as init when creating a new file; without --name,
  interactive setup asks for one and --yes uses substrate-dev
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
	case "init", "setup", "doctor", "plan", "provision", "images", "check", "prepare", "install", "demo", "client", "status", "destroy":
	default:
		return fmt.Errorf("unknown command %q; use --help", command)
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "deploy.json", "configuration file")
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
		fs.BoolVar(&azureChecks, "azure", false, "check Azure deployment permissions, AKS versions, SKU availability and CPU quota (requires --config)")
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
		fs.StringVar(&name, "name", "substrate-dev", "dedicated resource group and cluster name (interactive setup asks when omitted)")
		fs.StringVar(&location, "location", "eastus2", "Azure region")
	}
	if command == "setup" {
		fs.BoolVar(&yes, "yes", false, "accept billable deployment and select the kube-context without prompts")
		fs.BoolVar(&withDemo, "with-demo", false, "also deploy the counter microVM demo")
		fs.BoolVar(&accessible, "accessible", false, "use screen-reader-friendly prompts instead of interactive forms")
		fs.BoolVar(&skipContextHandoff, "skip-context-handoff", false, "skip importing/selecting a context and print private kubeconfig exports")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
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
		if _, err := os.Stat(*configPath); err == nil {
			return fmt.Errorf("%s already exists; edit it to change settings. init never overwrites an existing configuration: %w", *configPath, os.ErrExist)
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
		return initialize(*configPath, subscription, publisher, reader, cidr, name, location, unrestrictedAPI, &defaults.operator, publisherType, readerType, out)
	}
	if command == "setup" {
		var client installer.ClientSetup
		var configFlags, nameSet bool
		fs.Visit(func(f *flag.Flag) {
			nameSet = nameSet || f.Name == "name"
			switch f.Name {
			case "subscription", "identity-object-id", "asset-publisher-object-id", "identity-principal-type", "operator-object-id", "operator-principal-type", "snapshot-reader-object-id", "snapshot-reader-principal-type", "api-cidr", "allow-unrestricted-api", "name", "location":
				configFlags = true
			}
		})
		// Interactive setup asks for a name instead of prefilling the default.
		if !nameSet && !yes {
			name = ""
		}
		return setup(ctx, setupOptions{
			path: *configPath, subscription: subscription, publisher: publisher, reader: reader,
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
		explicitConfig := false
		fs.Visit(func(f *flag.Flag) { explicitConfig = explicitConfig || f.Name == "config" })
		if !explicitConfig {
			if azureChecks {
				return errors.New("doctor --azure requires --config")
			}
			if containerEngine != "" {
				return errors.New("doctor --container-engine needs --config for an installation-specific builder probe")
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
	c, err := installer.LoadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("reading deployment configuration %s: %w", *configPath, err)
	}
	lock, err := installer.Lock(c)
	if err != nil {
		return err
	}
	defer lock.Close()
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
	fmt.Fprintf(log, "[command] %s\n[config] %s\n[target] %s / %s\n", command, *configPath, c.Azure.ResourceGroup, c.Azure.Location)
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
	a := installer.App{Config: c, Runner: runner, ContainerEngine: containerEngine, Progress: p.step, Preview: func(changes []installer.ResourceChange) error {
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
			if err := a.ResetImageConfig(ctx, *configPath); err != nil {
				return err
			}
			p.pause()
			fmt.Fprintf(out, "Cleared saved image references in %s after confirming resource-group absence. Runtime asset settings are unchanged.\nNext: %s\n", *configPath, cliCommand("setup", "--config", *configPath))
			return nil
		}
		p.info("Builds and downloads can take several minutes. Failed runs may leave partial images in ACR.")
		result, err := a.BuildImages(ctx)
		if err != nil {
			return err
		}
		p.step("Saving image references and runtime asset paths")
		if err := installer.SaveBuildResult(*configPath, c, result); err != nil {
			return err
		}
		p.pause()
		fmt.Fprintf(out, "Updated %s with ACR image references and runtime assets.\n", *configPath)
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
		return a.Destroy(ctx, confirmation)
	}
	return nil
}

func initialize(path, subscription, publisher, reader, cidr, name, location string, unrestrictedAPI bool, operator *installer.OperatorConfig, publisherType, readerType string, out io.Writer) error {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	c := installer.DefaultConfig()
	c.Installation.ID = fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
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
	if err := c.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; edit it to change settings. init never overwrites an existing configuration: %w", path, err)
		}
		return err
	}
	_, writeErr := file.Write(append(data, '\n'))
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	printAPIAccess(out, c.AKS)
	fmt.Fprintf(out, "Created %s. No Azure resources were created.\nKeep installation.id unchanged for retries and teardown.\nNext: %s\n", path, cliCommand("setup", "--config", path))
	return nil
}
