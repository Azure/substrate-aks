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
	"time"

	"github.com/Azure/substrate-aks/internal/installer"
)

const usage = `substrate-aks <command> [flags]

An AKS packaging layer for Agent Substrate.
Azure infrastructure, declarative deployment configuration, and a guided installer.

Commands:
  setup      Guide provisioning, installation, and client setup
  init       Write a new configuration; creates no Azure resources
  doctor     Check local tools; makes no Azure resource calls
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

init flags:
  --subscription UUID                    Default: active Azure CLI subscription
  --identity-object-id UUID               Default: signed-in Azure CLI identity
  --api-cidr YOUR_IP/32                   Default: detected public IPv4 /32
  --allow-unrestricted-api               Opt out of the AKS API IP allowlist
  --snapshot-reader-object-id UUID        Optional read-only snapshot inspection
  --name substrate-dev --location eastus2

setup flags:
  Same configuration flags as init when creating a new file
  --yes                                  Accept billable deployment without prompts
  --with-demo                            Also deploy the counter microVM demo
  --accessible                           Use screen-reader-friendly prompts
`

func main() {
	ctx, cancel := interruptContext(context.Background())
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		console := newConsole(os.Stderr)
		if errors.Is(err, context.Canceled) {
			console.logger.Warn(err)
		} else {
			console.logger.Error(err)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out, stderr io.Writer) (resultErr error) {
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
	var yes, withDemo, unrestrictedAPI, accessible bool
	var registryOnly, checkImages, resetImages bool
	var containerEngine string
	if command == "setup" || command == "images" || command == "doctor" {
		fs.StringVar(&containerEngine, "container-engine", "", "image builder: auto (Docker/buildx then Podman), docker or podman")
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
	if command == "images" {
		fs.BoolVar(&checkImages, "check", false, "verify saved images without publishing or changing configuration")
		fs.BoolVar(&resetImages, "reset-config", false, "clear saved image references only after the installation resource group is absent")
	}
	if command == "init" || command == "setup" {
		fs.StringVar(&subscription, "subscription", "", "Azure subscription UUID (default: active Azure CLI subscription)")
		fs.StringVar(&publisher, "identity-object-id", "", "Azure identity Entra object UUID used for image/asset uploads (default: signed-in identity)")
		fs.StringVar(&publisher, "asset-publisher-object-id", "", "compatibility alias for --identity-object-id")
		fs.StringVar(&reader, "snapshot-reader-object-id", "", "optional Entra object UUID granted read-only snapshot access")
		fs.StringVar(&cidr, "api-cidr", "", "AKS API IPv4 CIDR (default: detected public IPv4 /32)")
		fs.BoolVar(&unrestrictedAPI, "allow-unrestricted-api", false, "allow internet-wide AKS API access instead of an IP allowlist")
		fs.StringVar(&name, "name", "substrate-dev", "dedicated resource group and cluster name")
		fs.StringVar(&location, "location", "eastus2", "Azure region")
	}
	if command == "setup" {
		fs.BoolVar(&yes, "yes", false, "accept billable deployment without interactive prompts")
		fs.BoolVar(&withDemo, "with-demo", false, "also deploy the counter microVM demo")
		fs.BoolVar(&accessible, "accessible", false, "use screen-reader-friendly prompts instead of interactive forms")
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
	if checkImages && resetImages {
		return errors.New("--check and --reset-config are mutually exclusive")
	}
	if command == "init" {
		if _, err := os.Stat(*configPath); err == nil {
			return fmt.Errorf("%s already exists; edit it to change settings. init never overwrites an existing configuration: %w", *configPath, os.ErrExist)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		defaults, err := discoverAzureDefaults(ctx, subscription, publisher, installer.ExecRunner{})
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
		return initialize(*configPath, subscription, publisher, reader, cidr, name, location, unrestrictedAPI, out)
	}
	if command == "setup" {
		var configFlags bool
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "subscription", "identity-object-id", "asset-publisher-object-id", "snapshot-reader-object-id", "api-cidr", "allow-unrestricted-api", "name", "location":
				configFlags = true
			}
		})
		return setup(ctx, setupOptions{
			path: *configPath, subscription: subscription, publisher: publisher, reader: reader,
			cidr: cidr, name: name, location: location, yes: yes, withDemo: withDemo,
			verbose: *verbose, configFlags: configFlags, unrestrictedAPI: unrestrictedAPI, accessible: accessible,
			containerEngine: containerEngine,
		}, os.Stdin, out, stderr, run)
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
	if checkImages {
		title = "Saved image verification"
	} else if resetImages {
		title = "Saved image reset"
	}
	p := newProgress(stderr, log, title, !*verbose && command != "status")
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
	if *verbose {
		runner.Verbose = p
		p.info("Diagnostic log: %s", log.Name())
	}
	a := installer.App{Config: c, Runner: runner, ContainerEngine: containerEngine, Progress: p.step, Preview: func(changes []installer.ResourceChange) error {
		p.pause()
		return writePlan(out, c.Azure.SubscriptionID, changes)
	}}
	switch command {
	case "doctor":
		return a.Doctor(ctx)
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
			fmt.Fprintf(out, "Cleared saved image references in %s after confirming resource-group absence. Runtime asset settings are unchanged.\nNext: substrate-aks setup --config %s\n", *configPath, shellQuote(*configPath))
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
		return printClientSetup(out, client)
	case "status":
		return a.Status(ctx)
	case "destroy":
		return a.Destroy(ctx, confirmation)
	}
	return nil
}

func initialize(path, subscription, publisher, reader, cidr, name, location string, unrestrictedAPI bool, out io.Writer) error {
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
		SnapshotReaderObjectID: reader,
		Location:               location, ResourceGroup: name,
	}
	c.AKS.ClusterName = name
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
	fmt.Fprintf(out, "Created %s. No Azure resources were created.\nKeep installation.id unchanged for retries and teardown.\nNext: substrate-aks setup --config %s\n", path, shellQuote(path))
	return nil
}
