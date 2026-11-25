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
	"strconv"
	"strings"
	"time"

	"github.com/Azure/substrate-aks/internal/installer"
	"github.com/charmbracelet/x/term"
)

type setupOptions struct {
	home, subscription, publisher, reader, cidr, name, location string
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
	// now reads the clock for setup timings; nil means time.Now.
	now func() time.Time
	// backgroundStarted, if set, is called when a stage starts in the
	// background, with a channel closed once its result is known. Tests
	// synchronize with it.
	backgroundStarted func(done <-chan struct{})
}

type commandRunner func(context.Context, []string, io.Writer, io.Writer) error

type setupStage struct {
	label, command string
	flags          []string
	// phase overrides the phase derived from command and flags.
	phase string
	// background runs the stage alongside the stages after it, which start
	// without waiting for it. The next stage with join waits for it.
	background, join bool
}

// phaseID returns the setup phase the stage belongs to.
func (s setupStage) phaseID() string {
	if s.phase != "" {
		return s.phase
	}
	return setupPhase(s.command, s.flags)
}

// setupSerialEnv names the diagnostic setting that runs every setup stage in
// order, as before image publishing overlapped AKS provisioning.
const setupSerialEnv = "SUBSTRATE_AKS_SERIAL_SETUP"

// setupSerial reports whether setupSerialEnv asks for serial order. An unset
// or unparsable value means overlap; note then describes a value that did not
// parse, or confirms serial order, so the diagnostic never fails silently.
func setupSerial() (serial bool, note string) {
	value, ok := os.LookupEnv(setupSerialEnv)
	if !ok || value == "" {
		return false, ""
	}
	serial, err := strconv.ParseBool(value)
	switch {
	case err != nil:
		return false, fmt.Sprintf("%s=%q is not a Boolean such as true, false, 1 or 0, so it is ignored: images publish while AKS is provisioned.", setupSerialEnv, value)
	case serial:
		return true, fmt.Sprintf("%s is set: images publish and are verified before AKS is previewed, one stage at a time (diagnostic).", setupSerialEnv)
	}
	return false, ""
}

func setupStages(c installer.Config, withDemo bool) []setupStage {
	var stages []setupStage
	fresh := c.Substrate.Images == (installer.ImagesConfig{})
	serial, _ := setupSerial()
	if fresh {
		stages = append(stages,
			setupStage{label: "Preview registry bootstrap", command: "plan", flags: []string{"--registry-only"}},
			setupStage{label: "Provision the private registry", command: "provision", flags: []string{"--registry-only"}})
	}
	publish := setupStage{label: "Publish images and prepare runtime assets", command: "images"}
	verify := setupStage{label: "Verify configured images before provisioning AKS", command: "images", flags: []string{"--check"}}
	cluster := []setupStage{
		{label: "Preview full infrastructure", command: "plan"},
		{label: "Provision AKS, storage, and identities", command: "provision"},
		{label: "Check cluster compatibility", command: "check"},
	}
	switch {
	case fresh && !serial:
		// Images need only the registry, so they publish while AKS is
		// previewed, approved and provisioned, and are verified just before
		// Substrate needs them.
		publish.background = true
		verify = setupStage{label: "Verify published images before installing Substrate", command: "images",
			flags: []string{"--check"}, phase: "substrate", join: true}
		stages = append(append(append(stages, publish), cluster...), verify)
	case fresh:
		stages = append(append(stages, publish, verify), cluster...)
	default:
		stages = append(append(stages, verify), cluster...)
	}
	stages = append(stages,
		setupStage{label: "Prepare Azure deployment manifests", command: "prepare"},
		setupStage{label: "Install Substrate and runtime assets", command: "install"},
		setupStage{label: "Inspect workload status", command: "status"})
	if withDemo {
		stages = append(stages, setupStage{label: "Deploy and warm the counter microVM demo", command: "demo"})
	}
	return append(stages, setupStage{label: "Set up kubectl and the matching kubectl-ate", command: "client"})
}

func setup(ctx context.Context, o setupOptions, in io.Reader, out, stderr io.Writer, execute commandRunner) error {
	if useSetupWorkspace(o, in, stderr) {
		return runSetupWorkspace(ctx, o, in, out, stderr, execute)
	}
	return setupFlow(ctx, o, in, out, stderr, execute)
}

func setupFlow(ctx context.Context, o setupOptions, in io.Reader, out, stderr io.Writer, execute commandRunner) (resultErr error) {
	timing := newSetupTimingRun(o.now)
	ctx = installer.WithTimings(ctx, timing.timings)
	defer func() { resultErr = timing.finish(resultErr) }()
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
	// The record as this run saved it, to recognize a later resume.
	var createdRecord []byte
	var statErr error
	if o.name != "" {
		_, statErr = os.Stat(installer.RecordPath(o.home, o.name))
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
	} else {
		statErr = os.ErrNotExist
	}
	if statErr == nil && o.configFlags {
		return existingSettingsError(o)
	}
	local := o.local
	if local == nil {
		local = installer.ExecRunner{}
	}
	var prior installer.Config
	if statErr == nil {
		var err error
		prior, err = installer.LoadInstallation(o.home, o.name)
		if err != nil {
			return err
		}
		if ui.session != nil {
			ui.session.send(setupTargetMsg{prior, o.withDemo})
		}
	}
	if ui.session != nil {
		log, err := openSetupPreparationLog(o.home, prior, statErr == nil)
		if err != nil {
			return fmt.Errorf("creating the setup log: %w", err)
		}
		defer log.Close()
		fmt.Fprintf(log, "[command] setup preparation\n[installation] %s\n", o.name)
		if o.discovery == nil {
			runner = installer.ExecRunner{Log: log}
		}
		if o.local == nil {
			local = installer.ExecRunner{Log: log}
		}
		ui.log = log
		ui.session.send(setupLogMsg(log.Name()))
	}
	// Fresh installations import release images, which need no local builder.
	needsBuilder := statErr == nil && prior.NeedsImageBuilder()
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
	engineLabel := "Not needed (release images)"
	if statErr == nil && prior.Substrate.Images != (installer.ImagesConfig{}) {
		engineLabel = "Not needed (reuse configured images)"
	}
	if needsBuilder {
		var err error
		engineLabel, err = checkImageBuilder(ctx, &o, prior, statErr != nil, local, prompt, ui)
		if err != nil {
			return err
		}
	}
	if statErr != nil {
		err := ui.check("Azure account / identities", func() error {
			var err error
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
		// Only a detected address is offered as a choice; an explicit flag
		// already decided API access.
		askAPI := o.cidr == "" && !o.unrestrictedAPI
		err = ui.check("API access address", func() error {
			o.cidr, err = resolveAPICIDR(ctx, o.cidr, o.unrestrictedAPI, o.ipClient)
			return err
		})
		if err != nil {
			if o.yes || ctx.Err() != nil || o.unrestrictedAPI {
				return err
			}
			askAPI = false
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
			prompt.nameAvailable = func(name string) error {
				if _, err := os.Stat(installer.RecordPath(o.home, name)); err == nil {
					return fmt.Errorf("installation %s already exists; run setup %s to resume it", name, name)
				}
				return nil
			}
			var open *bool
			if askAPI {
				open = new(bool)
			}
			o.name, o.location, err = prompt.installation(ctx, o.name, o.location, regions, o.cidr, open, read)
			if err != nil {
				return err
			}
			if open != nil && *open {
				o.cidr, o.unrestrictedAPI = "", true
			}
			if read != nil && *read {
				o.reader = o.publisher
			}
		}
		operator := installer.OperatorConfig{ObjectID: o.operatorID, PrincipalType: o.operatorType}
		if o.publisherType == "" && strings.EqualFold(o.publisher, defaults.signedInObjectID) {
			o.publisherType = defaults.signedInType
		}
		if err := initialize(o.home, o.subscription, o.publisher, o.reader, o.cidr, o.name, o.location, o.unrestrictedAPI, &operator, o.publisherType, o.readerType, io.Discard); err != nil {
			return err
		}
		created = true
		if createdRecord, err = os.ReadFile(installer.RecordPath(o.home, o.name)); err != nil {
			return err
		}
	}
	c, err := installer.LoadInstallation(o.home, o.name)
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
				return fmt.Errorf("%w; edit azure.location in %s before retrying", err, installer.RecordPath(o.home, o.name))
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
	ui.summary(c, defaults, installer.RecordPath(o.home, o.name), created, engineLabel)
	if !o.yes {
		accepted, err := prompt.confirm(ctx, "Continue with this billable installation?", false)
		if err != nil || !accepted {
			// Nothing exists in Azure yet, so an installation saved by this
			// run would only be a stale entry in list, unless another setup
			// has resumed it meanwhile.
			kept := false
			if created {
				discardErr := installer.DiscardNewInstallation(c, createdRecord)
				kept = errors.Is(discardErr, installer.ErrInstallationInUse)
				if !kept {
					err = errors.Join(err, discardErr)
				}
			}
			if err != nil {
				return err
			}
			if kept {
				return fmt.Errorf("setup cancelled before provisioning; another command is using installation %s, so it was kept", o.name)
			}
			if created {
				return errors.New("setup cancelled before provisioning; no Azure resources were created and the new installation was discarded")
			}
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
	if _, note := setupSerial(); note != "" && c.Substrate.Images == (installer.ImagesConfig{}) {
		ui.note(note)
	}
	stopped := func(label string, args []string, err error) error {
		reconcile := []string{"setup", o.name}
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
		return fmt.Errorf("setup stopped at %s: %w\n\nResources are retained.\nRetry failed stage: %s\nFull reconciliation (starts from the beginning, reusing configured images): %s\nTeardown:\n%s",
			label, err, cliCommand(args...), cliCommand(reconcile...),
			teardownCommand(o.name, c.Azure.ResourceGroup))
	}
	activePhase := ""
	// phaseDone lists the checks of the active phase that have finished, to
	// restore its checklist after a background stage is retried.
	var phaseDone []string
	phaseChecks := func(phase string) []string {
		var items []string
		for _, planned := range stages {
			if planned.phaseID() == phase {
				items = append(items, setupStageChecks(planned.label, planned.command, planned.flags)...)
			}
		}
		return items
	}
	timing.config = &c
	// bg is the background stage that has not been joined yet. While it
	// runs, setup holds the installation lock for every stage command.
	var bg *setupBackground
	stageCtx := func() context.Context {
		if bg != nil {
			return installer.WithHeldLock(ctx, c)
		}
		return ctx
	}
	stageArgs := func(command string, flags []string) []string {
		args := append([]string{command, o.name}, flags...)
		if o.containerEngine != "" && c.Substrate.Images == (installer.ImagesConfig{}) && (command == "doctor" || command == "images") {
			args = append(args, "--container-engine", o.containerEngine)
		}
		if o.verbose {
			args = append(args, "--verbose")
		}
		return args
	}
	// attempt runs a stage until it succeeds, offering a retry after each
	// failure. A non-nil failed starts at the retry offer.
	attempt := func(label, command string, flags, args []string, failed error) error {
		for {
			if failed != nil {
				if o.yes || ctx.Err() != nil || installer.Interrupted(failed) {
					return stopped(label, args, failed)
				}
				if ui.session == nil {
					ui.warning(failed.Error())
				}
				retry, promptErr := prompt.confirm(ctx, "Retry this stage with the same settings?", false)
				if promptErr != nil || !retry {
					return stopped(label, args, errors.Join(failed, promptErr))
				}
			}
			if err := ctx.Err(); err != nil {
				return stopped(label, args, err)
			}
			stageOut, stageErr := out, stderr
			if ui.session != nil {
				stageOut, stageErr = io.Discard, io.Discard
			}
			work := func() error {
				runCtx, stop := timing.timings.StageContext(stageCtx(), setupStageID(command, flags), label)
				err := execute(runCtx, args, stageOut, stageErr)
				stop(err)
				return err
			}
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
				return nil
			}
			failed = err
		}
	}
	// join waits for the background stage, or with wait false only collects
	// it if it has finished. It shows the stage's output, releases the lock
	// and, after a failure, stops setup or retries the stage in the
	// foreground.
	join := func(wait bool) error {
		if bg == nil || !wait && !bg.finished() {
			return nil
		}
		b := bg
		if !b.finished() {
			if ui.session == nil {
				ui.note("Waiting for image publishing to finish...")
			} else {
				ui.step("Waiting for image publishing to finish")
			}
		}
		err := b.wait()
		bg = nil
		b.close(ui, out, stderr)
		if err == nil {
			return nil
		}
		if o.yes || ctx.Err() != nil || installer.Interrupted(err) {
			return stopped(b.stage.label, b.args, err)
		}
		if ui.session != nil {
			// The checklist resets the body, so the warning that explains
			// the retry offer follows it.
			ui.checklist(b.stage.label, b.stage.label)
			ui.warning(fmt.Sprintf("%s failed: %v", b.stage.label, err))
		}
		err = attempt(b.stage.label, b.stage.command, b.stage.flags, b.args, err)
		if ui.session != nil {
			if err == nil {
				ui.session.send(setupBackgroundMsg{phase: b.stage.phaseID(), done: true})
			}
			ui.checklist(setupPhaseTitle(activePhase), phaseChecks(activePhase)...)
			for _, name := range phaseDone {
				ui.session.send(setupCheckMsg{name: name, done: true})
			}
		}
		return err
	}
	// A stage that stops setup leaves no background work running: the
	// background stage is cancelled and awaited, and any failure other than
	// that cancellation is reported after the foreground one.
	defer func() {
		if bg == nil {
			return
		}
		b := bg
		bg = nil
		b.cancel()
		err := b.wait()
		b.close(ui, out, stderr)
		if err != nil && !cancellationOnly(err) {
			resultErr = errors.Join(resultErr, fmt.Errorf("%s also failed: %w\nRetry: %s", b.stage.label, err, cliCommand(b.args...)))
		}
	}()
	enter := func(s setupStage) {
		phase := s.phaseID()
		timing.timings.Phase(phase, setupPhaseTitle(phase))
		if ui.session != nil {
			if phase != activePhase {
				activePhase, phaseDone = phase, nil
				ui.session.send(setupPhaseMsg(phase))
				ui.checklist(setupPhaseTitle(phase), phaseChecks(phase)...)
			}
		} else {
			ui.section(s.label)
		}
	}
	var stage func(s setupStage) error
	stage = func(s setupStage) error {
		if !s.join {
			enter(s)
		}
		args := stageArgs(s.command, s.flags)
		if err := attempt(s.label, s.command, s.flags, args, nil); err != nil {
			return err
		}
		phaseDone = append(phaseDone, setupStageChecks(s.label, s.command, s.flags)...)
		if s.command == "plan" && !o.yes {
			// A background failure is raised before the approval is used,
			// so a declined retry submits nothing. Its output and retry
			// follow the preview, so the preview runs again to sit next to
			// its question; it deploys nothing. The result is checked again
			// after an approval because images can fail while the prompt is
			// open. Successful output waits.
			imagesFailed := func() bool { return bg != nil && bg.finished() && bg.err != nil }
			repreview := func() error {
				if err := join(true); err != nil {
					return err
				}
				if ui.session == nil {
					ui.note("Previewing the infrastructure again, so the approval follows its preview.")
				}
				return stage(s)
			}
			if imagesFailed() {
				return repreview()
			}
			accepted, err := prompt.confirm(ctx, "Apply these planned changes?", false)
			if err != nil {
				return stopped(s.label, args, err)
			}
			if !accepted {
				return stopped(s.label, args, errors.New("no deployment was submitted for this stage"))
			}
			if imagesFailed() {
				return repreview()
			}
		}
		return nil
	}
	for _, s := range stages {
		if s.join {
			// The wait for the background stage belongs to the joining
			// stage's phase, not to the phase that ran alongside it.
			enter(s)
		}
		if err := join(s.join); err != nil {
			return err
		}
		if s.background {
			enter(s)
			b, err := startSetupBackground(ctx, c, timing.timings, s, stageArgs(s.command, s.flags), ui.session == nil, execute)
			if err == nil {
				bg = b
				if o.backgroundStarted != nil {
					o.backgroundStarted(b.done)
				}
				if ui.session != nil {
					ui.session.send(setupCheckMsg{name: s.label})
					ui.session.send(setupBackgroundMsg{phase: s.phaseID()})
				} else {
					ui.note("This stage continues in the background while setup provisions AKS; its output appears when it finishes.")
				}
				continue
			}
			// Another command holds the installation: run the stage in the
			// foreground, which reports that and offers a retry.
			if ui.session == nil {
				ui.note(fmt.Sprintf("Running this stage in the foreground: %v", err))
			}
			args := stageArgs(s.command, s.flags)
			if err := attempt(s.label, s.command, s.flags, args, nil); err != nil {
				return err
			}
			phaseDone = append(phaseDone, s.label)
			continue
		}
		if err := stage(s); err != nil {
			return err
		}
		if s.command == "images" && len(s.flags) > 0 && s.flags[0] == "--check" {
			ui.note("Configured images are available; image builds will not be repeated.")
		}
	}
	if o.client == nil || o.client.Kubeconfig == "" || o.client.BinDir == "" || o.client.Context != c.AKS.ClusterName {
		return fmt.Errorf("Substrate is installed, but client setup returned incomplete shell configuration.\nRetry: %s", cliCommand("client", o.name))
	}
	selected, handoffWarning, err := offerContextSwitch(ctx, o, c, prompt, ui)
	if err != nil {
		handoff := []string{"client", o.name, "--use-context"}
		if o.contextFile != "" {
			handoff = append(handoff, "--context-file", o.contextFile)
		}
		return stopped("Kubernetes context handoff", handoff, err)
	}
	steps := newSetupReceiptUI(ctx, out)
	timing.receipt = steps
	ui.complete(c.AKS.ClusterName, o.withDemo)
	if ui.session != nil {
		steps.complete(c.AKS.ClusterName, o.withDemo)
		// The workspace closes on success, so repeat why the kubeconfig was
		// left unchanged next to the exports that rely on it.
		if len(handoffWarning) > 0 {
			steps.warning(handoffWarning[0])
			for _, line := range handoffWarning[1:] {
				steps.note(line)
			}
		}
	}
	client := *o.client
	if selected != "" {
		client.Kubeconfig = selected
		ui.note("The selected kubeconfig now uses this installation's context. Apply the final printed exports in your current shell; PATH and shell profiles are unchanged.")
	} else {
		ui.note("Apply the printed shell configuration to use this installation's private kubeconfig.")
	}
	steps.section("# Connect from this shell:")
	if err := printClientSetup(out, client); err != nil {
		return err
	}
	if o.withDemo {
		steps.demo()
	}
	steps.section("# Delete all resources and persistent data when finished:")
	steps.command(teardownCommand(o.name, c.Azure.ResourceGroup))
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
		c.Installation.Dir, c.Installation.ID = dir, "probe"
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
		parts[i] = cliCommandArg(arg)
	}
	return strings.Join(parts, " ")
}

func cliCommandArg(arg string) string {
	if arg == "" || strings.IndexFunc(arg, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' || strings.ContainsRune("_@%+=:,./-", r))
	}) >= 0 {
		return shellQuote(arg)
	}
	return arg
}

func teardownCommand(name, resourceGroup string) string {
	return cliCommand("destroy", name, "--confirm-delete", resourceGroup)
}

func existingSettingsError(o setupOptions) error {
	return fmt.Errorf("installation %s already exists; setup configuration flags only apply to new installations. Edit %s to change its settings", o.name, installer.RecordPath(o.home, o.name))
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
// installation directory yet, so it logs under the installer home.
func openSetupPreparationLog(home string, prior installer.Config, saved bool) (*os.File, error) {
	if saved {
		return installer.NewLog(prior)
	}
	c := installer.DefaultConfig()
	c.Installation.Dir = home
	return installer.NewLog(c)
}
