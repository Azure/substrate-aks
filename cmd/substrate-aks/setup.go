package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Azure/substrate-aks/internal/installer"
	"github.com/charmbracelet/x/term"
)

type setupOptions struct {
	path, subscription, publisher, reader, cidr, name, location string
	yes, withDemo, verbose, configFlags, accessible             bool
	discovery                                                   installer.Runner
	unrestrictedAPI                                             bool
	ipClient                                                    *http.Client
	containerEngine                                             string
	local                                                       installer.Runner
	operatorID, operatorType                                    string
	contextFile                                                 string
}

type commandRunner func(context.Context, []string, io.Writer, io.Writer) error

func setup(ctx context.Context, o setupOptions, in io.Reader, out, stderr io.Writer, execute commandRunner) error {
	if file, ok := in.(*os.File); !o.yes && ok && !term.IsTerminal(file.Fd()) {
		return errors.New("setup needs an interactive terminal; use --yes with a complete configuration for automation")
	}
	ui := newSetupUI(stderr)
	prompt := newSetupPrompt(in, ui, o.accessible)
	ui.welcome(!prompt.accessible)
	runner := o.discovery
	if runner == nil {
		runner = installer.ExecRunner{}
	}
	var defaults azureDefaults
	created := false
	_, statErr := os.Stat(o.path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if statErr == nil && o.configFlags {
		return errors.New("setup configuration flags only create a new file; edit the existing deploy.json instead of overriding its settings")
	}
	local := o.local
	if local == nil {
		local = installer.ExecRunner{}
	}
	ui.section("1. Check local prerequisites")
	var prior installer.Config
	if statErr == nil {
		var err error
		prior, err = installer.LoadConfig(o.path)
		if err != nil {
			return err
		}
	}
	preflight := installer.App{Config: prior, Runner: local, Progress: func(format string, args ...any) { ui.note(fmt.Sprintf(format, args...)) }}
	if err := preflight.Doctor(ctx); err != nil {
		return fmt.Errorf("local prerequisites failed before Azure discovery or provisioning: %w", err)
	}
	if _, err := os.Stat(o.path); errors.Is(err, os.ErrNotExist) {
		ui.section("Discover Azure subscription and signed-in identity")
		defaults, err = discoverInstallationDefaults(ctx, o.subscription, o.publisher, o.operatorID, o.operatorType, runner)
		if err != nil {
			if o.yes || ctx.Err() != nil {
				return err
			}
			ui.warning(fmt.Sprintf("Azure defaults could not be fully discovered: %v", err))
			ui.note("Enter the missing IDs below.")
		}
		o.subscription, o.publisher = defaults.subscription, defaults.publisher
		o.operatorID, o.operatorType = defaults.operator.ObjectID, defaults.operator.PrincipalType
		o.cidr, err = resolveAPICIDR(ctx, o.cidr, o.unrestrictedAPI, o.ipClient)
		if err != nil {
			if o.yes || ctx.Err() != nil || o.unrestrictedAPI {
				return err
			}
			ui.warning(fmt.Sprintf("Public IPv4 could not be detected: %v", err))
			ui.note("Enter an allowed API CIDR below; access will not be opened automatically.")
		}
		if !o.yes {
			ui.section("Configure your installation")
			var err error
			for _, field := range []struct {
				label string
				value *string
				skip  bool
			}{
				{"Azure subscription UUID", &o.subscription, o.subscription != ""},
				{"Azure identity Entra object UUID", &o.publisher, o.publisher != ""},
				{"AKS operator Entra object UUID (cluster-scoped RBAC Cluster Admin)", &o.operatorID, o.operatorID != ""},
				{"Client public IPv4 CIDR allowed to reach AKS (for example 203.0.113.8/32)", &o.cidr, o.cidr != "" || o.unrestrictedAPI},
				{"Dedicated resource group and cluster name", &o.name, false},
			} {
				if field.skip {
					continue
				}
				*field.value, err = prompt.text(ctx, field.label, *field.value)
				if err != nil {
					return err
				}
			}
		}
		ui.note("Loading available Azure regions.")
		regions, err := readAzureRegions(ctx, o.subscription, runner)
		if err != nil {
			return err
		}
		o.location, err = validateAzureRegion(o.location, regions)
		if err != nil {
			return err
		}
		if !o.yes {
			o.location, err = prompt.region(ctx, o.location, regions)
			if err != nil {
				return err
			}
			if o.reader == "" {
				ui.section("Snapshot inspection")
				ui.warning("Snapshots can contain actor memory, files, and secrets.")
				ui.note("Runtime identities receive access automatically. Operator inspection is optional and read-only.")
				read, err := prompt.confirm(ctx, "Grant this Azure identity read-only snapshot inspection?", false)
				if err != nil {
					return err
				}
				if read {
					o.reader = o.publisher
				}
			}
		}
		operator := installer.OperatorConfig{ObjectID: o.operatorID, PrincipalType: o.operatorType}
		if err := initialize(o.path, o.subscription, o.publisher, o.reader, o.cidr, o.name, o.location, o.unrestrictedAPI, &operator, io.Discard); err != nil {
			return err
		}
		created = true
	} else if err != nil {
		return err
	} else if o.configFlags {
		return errors.New("setup configuration flags only create a new file; edit the existing deploy.json instead of overriding its settings")
	}
	c, err := installer.LoadConfig(o.path)
	if err != nil {
		return err
	}
	if c.Substrate.Images != (installer.ImagesConfig{}) {
		if err := c.ValidateInstall(); err != nil {
			return err
		}
	}
	if !created {
		ui.section("Validate saved Azure region")
		regions, err := readAzureRegions(ctx, c.Azure.SubscriptionID, runner)
		if err != nil {
			return err
		}
		if _, err := validateAzureRegion(c.Azure.Location, regions); err != nil {
			return fmt.Errorf("%w; edit azure.location in %s before retrying", err, o.path)
		}
	}
	if defaults.subscriptionName == "" {
		ui.note("Looking up the saved subscription name.")
		account, lookupErr := readAzureAccount(ctx, c.Azure.SubscriptionID, runner)
		if lookupErr == nil {
			if !strings.EqualFold(account.ID, c.Azure.SubscriptionID) || account.Name == "" {
				lookupErr = errors.New("Azure CLI returned a missing name or a different subscription ID")
			} else {
				defaults.subscriptionName = account.Name
			}
		}
		if lookupErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			ui.warning(fmt.Sprintf("Could not load the subscription name: %v", lookupErr))
			ui.note("The saved subscription GUID is shown below; the deployment target has not changed.")
		}
	}
	preflight.Config = c
	if err := preflight.RuntimePrerequisites(ctx); err != nil {
		return fmt.Errorf("runtime prerequisites failed before provisioning: %w", err)
	}
	engineLabel := "Not needed (reuse configured images)"
	if c.Substrate.Images == (installer.ImagesConfig{}) {
		ui.section("Image builder")
		builder := installer.App{Config: c, Runner: local, ContainerEngine: o.containerEngine,
			Progress: func(format string, args ...any) { ui.note(fmt.Sprintf(format, args...)) }}
		automatic := o.containerEngine == "" || o.containerEngine == "auto"
		var engine string
		mode := "explicit selection"
		if automatic && !o.yes {
			engines, err := builder.AvailableContainerEngines(ctx)
			if err != nil {
				return fmt.Errorf("checking the local image builders before provisioning: %w", err)
			}
			engine, mode = engines[0], "auto-detected"
			if len(engines) > 1 {
				engine, err = prompt.containerEngine(ctx)
				if err != nil {
					return err
				}
				mode = "selected"
			}
		} else {
			engine, err = builder.DetectContainerEngine(ctx)
			if err != nil {
				return fmt.Errorf("checking the local image builder before provisioning: %w", err)
			}
			if automatic {
				mode = "auto-detected"
				ui.note("Auto-detection prefers working Docker/buildx, then Podman.")
			}
		}
		engineLabel = engine + " (" + mode + ")"
		o.containerEngine = engine
	}
	ui.summary(c, defaults, o.path, created, engineLabel)
	if !o.yes {
		accepted, err := prompt.confirm(ctx, "Continue with this billable installation?", false)
		if err != nil {
			return err
		}
		if !accepted {
			return errors.New("setup cancelled before provisioning; no Azure resources were changed")
		}
		if !o.withDemo {
			o.withDemo, err = prompt.confirm(ctx, "Deploy the counter microVM demo after installation?", true)
			if err != nil {
				return err
			}
		}
	}
	stopped := func(label string, args []string, err error) error {
		reconcile := []string{"setup", "--config", o.path}
		if o.containerEngine != "" {
			reconcile = append(reconcile, "--container-engine", o.containerEngine)
		}
		if o.contextFile != "" {
			reconcile = append(reconcile, "--context-file", o.contextFile)
		}
		for _, option := range []struct {
			enabled bool
			flag    string
		}{{o.withDemo, "--with-demo"}, {o.verbose, "--verbose"}, {o.accessible, "--accessible"}, {o.yes, "--yes"}} {
			if option.enabled {
				reconcile = append(reconcile, option.flag)
			}
		}
		return fmt.Errorf("setup stopped at %s: %w\n\nResources are retained.\nRetry failed stage: %s\nFull reconciliation (starts from the beginning, reusing configured images): %s\nTeardown: %s",
			label, err, cliCommand(args...), cliCommand(reconcile...),
			cliCommand("destroy", "--config", o.path, "--confirm-delete", c.Azure.ResourceGroup))
	}
	stage := func(label, command string, flags ...string) error {
		ui.section(label)
		args := append([]string{command, "--config", o.path}, flags...)
		if c.Substrate.Images == (installer.ImagesConfig{}) && (command == "doctor" || command == "images") {
			args = append(args, "--container-engine", o.containerEngine)
		}
		if o.verbose {
			args = append(args, "--verbose")
		}
		for {
			if err := ctx.Err(); err != nil {
				return stopped(label, args, err)
			}
			err := execute(ctx, args, out, stderr)
			if err == nil {
				break
			}
			if o.yes || ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return stopped(label, args, err)
			}
			ui.warning(err.Error())
			retry, promptErr := prompt.confirm(ctx, "Retry this stage with the same settings?", false)
			if promptErr != nil || !retry {
				return stopped(label, args, errors.Join(err, promptErr))
			}
		}
		if command == "plan" && !o.yes {
			accepted, err := prompt.confirm(ctx, "Apply these planned changes?", false)
			if err != nil {
				return stopped(label, args, err)
			}
			if !accepted {
				return stopped(label, args, errors.New("no deployment was submitted for this stage"))
			}
		}
		return nil
	}
	if c.Substrate.Images == (installer.ImagesConfig{}) {
		for _, s := range []struct {
			label, command string
			flags          []string
		}{
			{"2. Preview registry bootstrap", "plan", []string{"--registry-only"}},
			{"3. Provision the private registry", "provision", []string{"--registry-only"}},
			{"4. Publish images and prepare runtime assets", "images", nil},
		} {
			if err := stage(s.label, s.command, s.flags...); err != nil {
				return err
			}
		}
	}
	if err := stage("Verify configured images before provisioning AKS", "images", "--check"); err != nil {
		return err
	}
	fmt.Fprintln(stderr, "\nConfigured images are available; image builds will not be repeated.")
	for _, s := range []struct{ label, command string }{
		{"5. Preview full infrastructure", "plan"},
		{"6. Provision AKS, storage, and identities", "provision"},
		{"7. Check cluster compatibility", "check"},
		{"8. Prepare Azure deployment manifests", "prepare"},
		{"9. Install Substrate and runtime assets", "install"},
		{"10. Inspect workload status", "status"},
	} {
		if err := stage(s.label, s.command); err != nil {
			return err
		}
	}
	if o.withDemo {
		if err := stage("11. Deploy and warm the counter microVM demo", "demo"); err != nil {
			return err
		}
	}
	if err := stage("12. Set up kubectl and the matching kubectl-ate", "client"); err != nil {
		return err
	}
	switched, err := offerContextSwitch(ctx, o, c, prompt, ui, out)
	if err != nil {
		return err
	}
	ui.complete(c.AKS.ClusterName, o.withDemo)
	if switched {
		ui.note("The selected kubeconfig now uses this installation's context. Apply the final printed exports in your current shell; PATH and shell profiles are unchanged.")
	} else {
		ui.note("Apply the printed shell configuration; the installer has not changed your default kubeconfig or PATH.")
	}
	steps := newSetupUI(out)
	if o.withDemo {
		steps.demo()
	}
	steps.section("# Delete all resources and persistent data when finished:")
	steps.command(cliCommand("destroy", "--config", o.path, "--confirm-delete", c.Azure.ResourceGroup))
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func cliCommand(args ...string) string {
	parts := []string{shellQuote(os.Args[0])}
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func printClientSetup(out io.Writer, c installer.ClientSetup) error {
	onPath := false
	plugin, err := exec.LookPath("kubectl-ate")
	if err == nil {
		found, err := os.Stat(plugin)
		if err != nil {
			return fmt.Errorf("checking kubectl-ate on PATH: %w", err)
		}
		installed, err := os.Stat(filepath.Join(c.BinDir, filepath.Base(plugin)))
		if err != nil {
			return fmt.Errorf("checking installed kubectl-ate: %w", err)
		}
		onPath = os.SameFile(found, installed)
	} else if !errors.Is(err, exec.ErrNotFound) && !errors.Is(err, exec.ErrDot) {
		return fmt.Errorf("locating kubectl-ate on PATH: %w", err)
	}
	if _, err := fmt.Fprintf(out, "# kubectl-ate installed in the Go binary directory: %s\n# Run this export in your current shell:\nexport KUBECONFIG=%s\n",
		shellQuote(c.BinDir), shellQuote(c.Kubeconfig)); err != nil {
		return err
	}
	if !onPath {
		if _, err := fmt.Fprintf(out, "# Put the installed plugin first on PATH so kubectl can find it:\nexport PATH=%s:\"$PATH\"\n", shellQuote(c.BinDir)); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(out, "\n# Inspect the installation:\nkubectl --context %s get pods --all-namespaces\nkubectl ate --help\n", shellQuote(c.Context))
	return err
}
