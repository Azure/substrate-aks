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
	azure                                                       installer.Runner
	operatorID, operatorType                                    string
	publisherType, readerType                                   string
	contextFile                                                 string
	skipContextHandoff                                          bool
	client                                                      *installer.ClientSetup
}

type commandRunner func(context.Context, []string, io.Writer, io.Writer) error

type setupStage struct {
	label, command string
	flags          []string
}

func setupStages(c installer.Config, withDemo bool) []setupStage {
	var stages []setupStage
	if c.Substrate.Images == (installer.ImagesConfig{}) {
		stages = append(stages,
			setupStage{"Preview registry bootstrap", "plan", []string{"--registry-only"}},
			setupStage{"Provision the private registry", "provision", []string{"--registry-only"}},
			setupStage{"Publish images and prepare runtime assets", "images", nil})
	}
	stages = append(stages,
		setupStage{"Verify configured images before provisioning AKS", "images", []string{"--check"}},
		setupStage{"Preview full infrastructure", "plan", nil},
		setupStage{"Provision AKS, storage, and identities", "provision", nil},
		setupStage{"Check cluster compatibility", "check", nil},
		setupStage{"Prepare Azure deployment manifests", "prepare", nil},
		setupStage{"Install Substrate and runtime assets", "install", nil},
		setupStage{"Inspect workload status", "status", nil})
	if withDemo {
		stages = append(stages, setupStage{"Deploy and warm the counter microVM demo", "demo", nil})
	}
	return append(stages, setupStage{"Set up kubectl and the matching kubectl-ate", "client", nil})
}

func setup(ctx context.Context, o setupOptions, in io.Reader, out, stderr io.Writer, execute commandRunner) error {
	if useSetupWorkspace(o, in, stderr) {
		return runSetupWorkspace(ctx, o, in, out, stderr, execute)
	}
	return setupFlow(ctx, o, in, out, stderr, execute)
}

func setupFlow(ctx context.Context, o setupOptions, in io.Reader, out, stderr io.Writer, execute commandRunner) error {
	if file, ok := in.(*os.File); !o.yes && ok && !term.IsTerminal(file.Fd()) {
		return errors.New("setup needs an interactive terminal; use --yes with a complete configuration for automation")
	}
	var ui *setupUI
	if session := setupSessionFrom(ctx); session != nil && session.ui != nil {
		copied := *session.ui
		ui = &copied
	} else {
		ui = newSetupUI(stderr)
	}
	ui.session = setupSessionFrom(ctx)
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
	var prior installer.Config
	if statErr == nil {
		var err error
		prior, err = installer.LoadConfig(o.path)
		if err != nil {
			return err
		}
		if ui.session != nil {
			ui.session.send(setupTargetMsg{prior, o.withDemo})
		}
	}
	if ui.session != nil {
		log, err := openSetupPreparationLog(o.path, prior, statErr == nil)
		if err != nil {
			return fmt.Errorf("creating the setup log: %w", err)
		}
		defer log.Close()
		fmt.Fprintf(log, "[command] setup preparation\n[config] %s\n", o.path)
		if o.discovery == nil {
			runner = installer.ExecRunner{Log: log}
		}
		if o.local == nil {
			local = installer.ExecRunner{Log: log}
		}
		ui.log = log
		ui.session.send(setupLogMsg(log.Name()))
	}
	needsBuilder := statErr != nil || prior.Substrate.Images == (installer.ImagesConfig{})
	environment := []string{"Local tools"}
	if needsBuilder {
		environment = append(environment, "Image builder")
	}
	if errors.Is(statErr, os.ErrNotExist) {
		environment = append(environment, "Azure account / identities", "API access address", "Azure regions")
	}
	ui.checklist("Check your environment", environment...)
	preflight := installer.App{Config: prior, Runner: local, Progress: func(format string, args ...any) { ui.step(fmt.Sprintf(format, args...)) }}
	if err := ui.check("Local tools", func() error { return preflight.Doctor(ctx) }); err != nil {
		return fmt.Errorf("local prerequisites failed before Azure discovery or provisioning: %w", err)
	}
	engineLabel := "Not needed (reuse configured images)"
	if needsBuilder {
		var err error
		engineLabel, err = checkImageBuilder(ctx, &o, prior, statErr != nil, local, prompt, ui)
		if err != nil {
			return err
		}
	}
	if _, err := os.Stat(o.path); errors.Is(err, os.ErrNotExist) {
		err = ui.check("Azure account / identities", func() error {
			defaults, err = discoverInstallationDefaults(ctx, o.subscription, o.publisher, o.operatorID, o.operatorType, runner)
			return err
		})
		if err != nil {
			if o.yes || ctx.Err() != nil {
				return err
			}
			ui.warning(fmt.Sprintf("Azure defaults could not be fully discovered: %v", err))
			ui.note("Enter the missing IDs below.")
		}
		o.subscription, o.publisher = defaults.subscription, defaults.publisher
		o.operatorID, o.operatorType = defaults.operator.ObjectID, defaults.operator.PrincipalType
		if ui.session != nil && defaults.subscriptionName != "" {
			ui.session.send(setupSubscriptionMsg{defaults.subscription, defaults.subscriptionName})
		}
		var regions []string
		// Regions need a subscription; if discovery found none, this check
		// runs after it is entered below.
		loadRegions := func() error {
			return ui.check("Azure regions", func() error {
				var err error
				regions, err = readAzureRegions(ctx, o.subscription, runner)
				return err
			})
		}
		err = ui.check("API access address", func() error {
			o.cidr, err = resolveAPICIDR(ctx, o.cidr, o.unrestrictedAPI, o.ipClient)
			return err
		})
		if err != nil {
			if o.yes || ctx.Err() != nil || o.unrestrictedAPI {
				return err
			}
			ui.warning(fmt.Sprintf("Public IPv4 could not be detected: %v", err))
			ui.note("Enter an allowed API CIDR below; access will not be opened automatically.")
		}
		if o.subscription != "" {
			if err := loadRegions(); err != nil {
				return err
			}
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
		if regions == nil {
			if err := loadRegions(); err != nil {
				return err
			}
		}
		o.location, err = validateAzureRegion(o.location, regions)
		if err != nil {
			return err
		}
		if !o.yes {
			ui.section("Configure your installation")
			var read *bool
			if o.reader == "" {
				read = new(bool)
			}
			o.name, o.location, err = prompt.installation(ctx, o.name, o.location, regions, read)
			if err != nil {
				return err
			}
			if read != nil && *read {
				o.reader = o.publisher
			}
		}
		operator := installer.OperatorConfig{ObjectID: o.operatorID, PrincipalType: o.operatorType}
		if o.publisherType == "" && strings.EqualFold(o.publisher, defaults.signedInObjectID) {
			o.publisherType = defaults.signedInType
		}
		if err := initialize(o.path, o.subscription, o.publisher, o.reader, o.cidr, o.name, o.location, o.unrestrictedAPI, &operator, o.publisherType, o.readerType, io.Discard); err != nil {
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
	if ui.session != nil {
		ui.session.send(setupTargetMsg{c, o.withDemo})
	}
	if c.Substrate.Images != (installer.ImagesConfig{}) {
		if err := c.ValidateInstall(); err != nil {
			return err
		}
	}
	validation := []string{}
	if !created {
		validation = append(validation, "Saved Azure region")
	}
	if defaults.subscriptionName == "" {
		validation = append(validation, "Azure subscription")
	}
	validation = append(validation, "Runtime tools", "Azure deployment readiness", "Operator identity review")
	ui.checklist("Validate your deployment", validation...)
	if !created {
		if err := ui.check("Saved Azure region", func() error {
			regions, err := readAzureRegions(ctx, c.Azure.SubscriptionID, runner)
			if err != nil {
				return err
			}
			if _, err := validateAzureRegion(c.Azure.Location, regions); err != nil {
				return fmt.Errorf("%w; edit azure.location in %s before retrying", err, o.path)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if defaults.subscriptionName == "" {
		lookupErr := ui.check("Azure subscription", func() error {
			account, err := readAzureAccount(ctx, c.Azure.SubscriptionID, runner)
			if err != nil {
				return err
			}
			if !strings.EqualFold(account.ID, c.Azure.SubscriptionID) || account.Name == "" {
				return errors.New("Azure CLI returned a missing name or a different subscription ID")
			}
			defaults.subscriptionName = account.Name
			if ui.session != nil {
				ui.session.send(setupSubscriptionMsg{account.ID, account.Name})
			}
			return nil
		})
		if lookupErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			ui.warning(fmt.Sprintf("Could not load the subscription name: %v", lookupErr))
			ui.note("The saved subscription GUID is shown below; the deployment target has not changed.")
		}
	}
	preflight.Config = c
	if err := ui.check("Runtime tools", func() error { return preflight.RuntimePrerequisites(ctx) }); err != nil {
		return fmt.Errorf("runtime prerequisites failed before provisioning: %w", err)
	}
	cloudRunner := o.azure
	if cloudRunner == nil {
		cloudRunner = runner
	}
	cloud := installer.App{Config: c, Runner: cloudRunner, Progress: func(format string, args ...any) { ui.step(fmt.Sprintf(format, args...)) }}
	if err := ui.check("Azure deployment readiness", func() error { return cloud.CloudPreflight(ctx) }); err != nil {
		return fmt.Errorf("Azure prerequisites failed before provisioning: %w", err)
	}
	if err := ui.check("Operator identity review", func() error { return warnOperatorIdentity(ctx, c, defaults, runner, ui) }); err != nil {
		return err
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
	if ui.session != nil {
		ui.session.send(setupTargetMsg{c, o.withDemo})
	}
	// Each stage writes its own installation log from here on.
	ui.log = nil
	stages := setupStages(c, o.withDemo)
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
		}{{o.withDemo, "--with-demo"}, {o.verbose, "--verbose"}, {o.accessible, "--accessible"}, {o.yes, "--yes"}, {o.skipContextHandoff, "--skip-context-handoff"}} {
			if option.enabled {
				reconcile = append(reconcile, option.flag)
			}
		}
		return fmt.Errorf("setup stopped at %s: %w\n\nResources are retained.\nRetry failed stage: %s\nFull reconciliation (starts from the beginning, reusing configured images): %s\nTeardown: %s",
			label, err, cliCommand(args...), cliCommand(reconcile...),
			cliCommand("destroy", "--config", o.path, "--confirm-delete", c.Azure.ResourceGroup))
	}
	activePhase := ""
	stage := func(label, command string, flags ...string) error {
		if ui.session != nil {
			phase := setupPhase(command, flags)
			if phase != activePhase {
				activePhase = phase
				ui.session.send(setupPhaseMsg(phase))
				var items []string
				for _, planned := range stages {
					if setupPhase(planned.command, planned.flags) == phase {
						items = append(items, setupStageChecks(planned.label, planned.command, planned.flags)...)
					}
				}
				ui.checklist(setupPhaseTitle(phase), items...)
			}
		} else {
			ui.section(label)
		}
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
			stageOut, stageErr := out, stderr
			if ui.session != nil {
				stageOut, stageErr = io.Discard, io.Discard
			}
			work := func() error { return execute(ctx, args, stageOut, stageErr) }
			var err error
			if ui.session != nil {
				items := setupStageChecks(label, command, flags)
				if len(items) > 1 {
					err = ui.checkGroup(items, work)
				} else {
					err = ui.check(label, work)
				}
			} else {
				err = work()
			}
			if err == nil {
				break
			}
			if o.yes || ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return stopped(label, args, err)
			}
			if ui.session == nil {
				ui.warning(err.Error())
			}
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
	for _, s := range stages {
		if err := stage(s.label, s.command, s.flags...); err != nil {
			return err
		}
		if s.command == "images" && len(s.flags) > 0 && s.flags[0] == "--check" {
			ui.note("Configured images are available; image builds will not be repeated.")
		}
	}
	if o.client == nil || o.client.Kubeconfig == "" || o.client.BinDir == "" || o.client.Context != c.AKS.ClusterName {
		return fmt.Errorf("Substrate is installed, but client setup returned incomplete shell configuration.\nRetry: %s", cliCommand("client", "--config", o.path))
	}
	selected, err := offerContextSwitch(ctx, o, c, prompt, ui)
	if err != nil {
		handoff := []string{"client", "--config", o.path, "--use-context"}
		if o.contextFile != "" {
			handoff = append(handoff, "--context-file", o.contextFile)
		}
		return stopped("Kubernetes context handoff", handoff, err)
	}
	steps := newSetupReceiptUI(ctx, out)
	ui.complete(c.AKS.ClusterName, o.withDemo)
	if ui.session != nil {
		steps.complete(c.AKS.ClusterName, o.withDemo)
	}
	client := *o.client
	if selected != "" {
		client.Kubeconfig = selected
		ui.note("The selected kubeconfig now uses this installation's context. Apply the final printed exports in your current shell; PATH and shell profiles are unchanged.")
	} else {
		ui.note("Apply the printed shell configuration; the installer has not changed your default kubeconfig or PATH.")
	}
	steps.section("# Connect from this shell:")
	if err := printClientSetup(out, client); err != nil {
		return err
	}
	if o.withDemo {
		steps.demo()
	}
	steps.section("# Delete all resources and persistent data when finished:")
	steps.command(cliCommand("destroy", "--config", o.path, "--confirm-delete", c.Azure.ResourceGroup))
	return nil
}

// checkImageBuilder pins the image builder during the local environment check,
// offering a choice when auto-detection finds both. A new installation has no
// workspace yet, so it probes from a throwaway one: the probe copies only the
// caller's Docker context and plugin settings, and buildx state stays shared.
func checkImageBuilder(ctx context.Context, o *setupOptions, c installer.Config, fresh bool, local installer.Runner, prompt *setupPrompt, ui *setupUI) (string, error) {
	if fresh {
		dir, err := os.MkdirTemp("", "substrate-aks-builder-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(dir)
		c = installer.DefaultConfig()
		c.Installation.StateDir, c.Installation.ID = dir, "probe"
	}
	var label string
	err := ui.check("Image builder", func() error {
		builder := installer.App{Config: c, Runner: local, ContainerEngine: o.containerEngine,
			Progress: func(format string, args ...any) { ui.step(fmt.Sprintf(format, args...)) }}
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
			var err error
			engine, err = builder.DetectContainerEngine(ctx)
			if err != nil {
				return fmt.Errorf("checking the local image builder before provisioning: %w", err)
			}
			if automatic {
				mode = "auto-detected"
				ui.note("Auto-detection prefers working Docker/buildx, then Podman.")
			}
		}
		label = engine + " (" + mode + ")"
		o.containerEngine = engine
		return nil
	})
	return label, err
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func cliCommand(args ...string) string {
	parts := append([]string{os.Args[0]}, args...)
	for i, arg := range parts {
		if arg == "" || strings.IndexFunc(arg, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
				r >= '0' && r <= '9' || strings.ContainsRune("_@%+=:,./-", r))
		}) >= 0 {
			parts[i] = shellQuote(arg)
		}
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

// openSetupPreparationLog records the environment checks that run before any
// stage command. A saved installation logs with its stages; a new one has no
// installation ID yet, so it logs under the default state directory beside the
// configuration.
func openSetupPreparationLog(path string, prior installer.Config, saved bool) (*os.File, error) {
	if saved {
		return installer.NewLog(prior)
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	c := installer.DefaultConfig()
	c.Installation.StateDir = filepath.Join(dir, c.Installation.StateDir)
	return installer.NewLog(c)
}
