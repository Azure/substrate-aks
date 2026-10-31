package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/substrate-aks/internal/installer"
)

func subscriptionDisplayRunner(t *testing.T) installer.Runner {
	t.Helper()
	return defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
		if isRegionList(c) {
			return []byte(testRegionsJSON), nil
		}
		switch strings.Join(c.Args, " ") {
		case "az account show --only-show-errors --output json":
			return []byte(accountJSON("user")), nil
		case "az ad signed-in-user show --only-show-errors --output json":
			return []byte(fmt.Sprintf(`{"id":%q}`, defaultPublisher)), nil
		}
		want := "az account show --only-show-errors --output json --subscription " + defaultSubscription
		if strings.Join(c.Args, " ") != want {
			t.Fatalf("subscription display consulted the wrong context: %v", c.Args)
		}
		return []byte(accountJSON("user")), nil
	})
}

// errFirstStage ends an approved setup at its first stage so a test can
// inspect the installation it saved; a declined setup discards a new one.
var errFirstStage = errors.New("stopped at the first stage")

func stopAtFirstStage(context.Context, []string, io.Writer, io.Writer) error { return errFirstStage }

func setupForTest(ctx context.Context, o setupOptions, in io.Reader, out, stderr io.Writer, execute commandRunner) error {
	client := installer.ClientSetup{}
	if o.azure == nil {
		o.azure = successfulCloudRunner(o.home)
	}
	o.client = &client
	if o.contextFile == "" && !o.skipContextHandoff {
		o.contextFile = filepath.Join(o.home, "operator-kubeconfig")
	}
	if o.subscription != "" && o.publisher != "" && o.operatorID == "" {
		o.operatorID = defaultPublisher
	}
	if o.local == nil {
		o.local = defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
			switch strings.Join(c.Args, " ") {
			case "docker info --format {{json .ServerVersion}}", "docker buildx version":
				return nil, nil
			case "docker buildx inspect --bootstrap":
				return []byte("Status: running\n"), nil
			case "podman info --format json":
				if o.containerEngine == "podman" {
					return nil, nil
				}
				return nil, errors.New("Podman unavailable in this fixture")
			default:
				return nil, fmt.Errorf("unexpected builder probe: %v", c.Args)
			}
		})
	}
	builder := o.local
	o.local = defaultsRunner(func(ctx context.Context, c installer.Command) ([]byte, error) {
		if c.Args[0] == "docker" || c.Args[0] == "podman" {
			return builder.Run(ctx, c)
		}
		if c.Args[0] == "kubectl" {
			return []byte(`{"clientVersion":{"major":"1","minor":"37"}}`), nil
		}
		return nil, nil
	})
	return setup(ctx, o, in, out, stderr, func(ctx context.Context, args []string, out, stderr io.Writer) error {
		if err := execute(ctx, args, out, stderr); err != nil {
			return err
		}
		if args[0] != "client" {
			return nil
		}
		c, err := installer.LoadConfig(installer.RecordPath(o.home, o.name))
		if err != nil {
			return err
		}
		client = installer.ClientSetup{
			Kubeconfig: filepath.Join(c.Installation.Dir, "kubeconfig"),
			Context:    c.AKS.ClusterName,
			BinDir:     o.home,
		}
		if err := os.WriteFile(filepath.Join(client.BinDir, "kubectl-ate"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(c.Installation.Dir, "kubeconfig")); !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return writeTestClientKubeconfig(c)
	})
}

func testRuntimeAssets(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"cloud-hypervisor", "virtiofsd", "vmlinux", "rootfs.img"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("asset"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSetupSequencesStagesAndStopsOnFailure(t *testing.T) {
	for _, fail := range []string{"", "images", "demo", "client"} {
		t.Run("failure="+fail, func(t *testing.T) {
			path := testCLIConfig(t)
			config, loadErr := installer.LoadConfig(path)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			wantTeardown := teardownCommand(config.Installation.Name, config.Azure.ResourceGroup)
			var commands []string
			publications, imageChecks := 0, 0
			want := errors.New("publisher failed")
			execute := func(_ context.Context, args []string, _, _ io.Writer) error {
				commands = append(commands, args[0])
				if args[0] == "images" {
					if strings.Contains(strings.Join(args, " "), "--check") {
						imageChecks++
					} else {
						publications++
					}
				}
				if len(args) < 2 || args[1] != config.Installation.Name {
					t.Fatalf("stage lost installation: %v", args)
				}
				if args[0] == fail {
					return want
				}
				return nil
			}
			var output bytes.Buffer
			err := setupForTest(t.Context(), setupOptions{home: recordHome(path), name: recordName(path), yes: true, withDemo: true, discovery: subscriptionDisplayRunner(t)},
				strings.NewReader(""), &output, &output, execute)
			wantCommands := []string{"plan", "provision", "images", "images", "plan", "provision", "check", "prepare", "install", "status", "demo", "client"}
			if i := slices.Index(wantCommands, fail); i >= 0 {
				wantCommands = wantCommands[:i+1]
			}
			if !slices.Equal(commands, wantCommands) {
				t.Fatalf("setup lost a required stage, changed order or continued after failure: %v", commands)
			}
			if fail != "" {
				if !errors.Is(err, want) || !strings.Contains(err.Error(), "Resources are retained") ||
					!strings.Contains(err.Error(), "Teardown:\n"+wantTeardown) {
					t.Fatalf("stage failure lost retry/cleanup guidance: %v", err)
				}
				if strings.Contains(output.String(), "Setup complete.") || strings.Contains(output.String(), "# Try the counter demo:") {
					t.Fatalf("setup printed successful completion or demo next steps after failure:\n%s", &output)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if strings.Count(output.String(), "DONE  Setup complete.") != 1 ||
					!strings.Contains(output.String(), "Counter microVM demo installed.") ||
					!strings.Contains(output.String(), wantTeardown+"\n") {
					t.Fatalf("successful setup did not print its final completion banner:\n%s", &output)
				}
				if publications != 1 || imageChecks != 1 {
					t.Fatalf("fresh setup must publish once and verify before provisioning AKS: publications=%d, checks=%d", publications, imageChecks)
				}
			}
		})
	}
}

func TestSetupForwardsContainerEngineOnlyToBuildPrerequisitesAndImages(t *testing.T) {
	path := testCLIConfig(t)
	var output bytes.Buffer
	err := setupForTest(t.Context(), setupOptions{home: recordHome(path), name: recordName(path), yes: true, containerEngine: "podman", discovery: subscriptionDisplayRunner(t)},
		strings.NewReader(""), &output, &output, func(_ context.Context, args []string, _, _ io.Writer) error {
			hasEngine := strings.Contains(strings.Join(args, " "), "--container-engine podman")
			if hasEngine != (args[0] == "doctor" || args[0] == "images") {
				t.Fatalf("setup lost engine selection or passed it to an unrelated stage: %v", args)
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
}

func TestContainerEngineRejectsInvalidSelectionBeforeConfigOrCloudAccess(t *testing.T) {
	var output bytes.Buffer
	err := run(t.Context(), []string{"setup", "missing", "--container-engine", "unknown"}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "unknown container engine") {
		t.Fatalf("invalid engine selection continued setup: %v", err)
	}
}

func TestSetupShowsEngineBeforeBillingConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, requested, available, label string
	}{
		{"Docker only", "", "docker", "docker (auto-detected)"},
		{"Podman only", "", "podman", "podman (auto-detected)"},
		{"explicit Podman", "podman", "podman", "podman (explicit selection)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := testCLIConfig(t)
			var output bytes.Buffer
			err := setupForTest(t.Context(), setupOptions{
				home: recordHome(path), name: recordName(path), containerEngine: tc.requested, discovery: subscriptionDisplayRunner(t),
				local: defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
					if c.Args[0] != tc.available {
						return nil, errors.New("builder unavailable in this fixture")
					}
					return []byte("Status: running\n"), nil
				}),
			}, strings.NewReader("no\n"), &output, &output, func(context.Context, []string, io.Writer, io.Writer) error {
				t.Fatal("declined setup executed an installation stage")
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "before provisioning") {
				t.Fatalf("setup did not reach billing confirmation: %v", err)
			}
			text := strings.Join(strings.Fields(output.String()), " ")
			selected, confirmation := strings.Index(text, tc.label), strings.Index(text, "Continue with this billable installation?")
			if selected < 0 || confirmation < selected {
				t.Fatalf("selected engine was not disclosed before confirmation:\n%s", text)
			}
			if strings.Contains(text, "Image builder: docker or podman") {
				t.Fatalf("setup prompted despite only one usable engine or an explicit selection:\n%s", text)
			}
		})
	}
}

func TestSetupOffersBothEnginesAndPinsInteractiveChoice(t *testing.T) {
	for _, selection := range []string{"", "podman"} {
		t.Run("selection="+selection, func(t *testing.T) {
			path := testCLIConfig(t)
			var output bytes.Buffer
			var probes []string
			engine := selection
			if engine == "" {
				engine = "docker"
			}
			err := setupForTest(t.Context(), setupOptions{
				home: recordHome(path), name: recordName(path), discovery: subscriptionDisplayRunner(t),
				local: defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
					probes = append(probes, strings.Join(c.Args, " "))
					return []byte("Status: running\n"), nil
				}),
			}, strings.NewReader(selection+"\nyes\nno\nyes\nyes\n"), &output, &output,
				func(_ context.Context, args []string, _, _ io.Writer) error {
					if (args[0] == "doctor" || args[0] == "images") &&
						!strings.Contains(strings.Join(args, " "), "--container-engine "+engine) {
						t.Fatalf("setup ignored interactive engine selection: %v", args)
					}
					return nil
				})
			if err != nil {
				t.Fatal(err)
			}
			text := strings.Join(strings.Fields(output.String()), " ")
			picker := strings.Index(text, "Image builder: docker or podman [docker]")
			confirmation := strings.Index(text, "Continue with this billable installation?")
			if picker < 0 || confirmation < picker || !strings.Contains(text, engine+" (selected)") ||
				strings.Join(probes, ",") != "docker info --format {{json .ServerVersion}},docker buildx version,docker buildx inspect --bootstrap,podman info --format json" {
				t.Fatalf("conditional picker, summary, or engine discovery incorrect: %v\n%s", probes, text)
			}
		})
	}
}

func TestSetupSkipsEnginePickerForAutomationAndExplicitSelection(t *testing.T) {
	for _, requested := range []string{"", "docker", "podman"} {
		t.Run("requested="+requested, func(t *testing.T) {
			path := testCLIConfig(t)
			var output bytes.Buffer
			var probes []string
			yes, answers, engine := requested == "", "no\n", requested
			if yes {
				answers, engine = "", "docker"
			}
			err := setupForTest(t.Context(), setupOptions{
				home: recordHome(path), name: recordName(path), yes: yes, containerEngine: requested, discovery: subscriptionDisplayRunner(t),
				local: defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
					probes = append(probes, c.Args[0])
					return []byte("Status: running\n"), nil
				}),
			}, strings.NewReader(answers), &output, &output,
				func(_ context.Context, args []string, _, _ io.Writer) error {
					if (args[0] == "doctor" || args[0] == "images") &&
						!strings.Contains(strings.Join(args, " "), "--container-engine "+engine) {
						t.Fatalf("automation did not keep default engine: %v", args)
					}
					return nil
				})
			if yes && err != nil || !yes && (err == nil || !strings.Contains(err.Error(), "before provisioning")) {
				t.Fatalf("unexpected setup result: %v", err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("declining a resumed installation removed its saved record: %v", err)
			}
			if strings.Contains(output.String(), "Image builder: docker or podman") {
				t.Fatal("setup prompted despite automation or explicit selection")
			}
			for _, probe := range probes {
				if probe != engine {
					t.Fatalf("setup probed unnecessary engine %q: %v", probe, probes)
				}
			}
		})
	}
}

func TestNewInstallationChoosesEngineBeforeAzureDiscovery(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	path := testRecord(t, "substrate-test")
	var output bytes.Buffer
	var calls []string
	stop := errors.New("stop after discovery")
	err := setupForTest(t.Context(), setupOptions{
		home: recordHome(path), name: recordName(path), cidr: "203.0.113.8/32",
		discovery: defaultsRunner(func(context.Context, installer.Command) ([]byte, error) {
			calls = append(calls, "azure")
			return nil, stop
		}),
		local: defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
			calls = append(calls, c.Args[0])
			return []byte("Status: running\n"), nil
		}),
	}, strings.NewReader("podman\n"), &output, &output, func(context.Context, []string, io.Writer, io.Writer) error {
		t.Fatal("unfinished setup reached installation stages")
		return nil
	})
	if err == nil {
		t.Fatal("setup continued without the remaining answers")
	}
	text := strings.Join(strings.Fields(output.String()), " ")
	picker := strings.Index(text, "Image builder: docker or podman [docker]")
	first := slices.Index(calls, "azure")
	if picker < 0 || first < 0 || slices.Contains(calls[first:], "docker") || slices.Contains(calls[first:], "podman") {
		t.Fatalf("new installation did not choose its builder before Azure discovery: %v\n%s", calls, text)
	}
	if left, _ := os.ReadDir(scratch); len(left) != 0 {
		t.Fatalf("builder probe left its throwaway workspace behind: %v", left)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("builder probe created the installation before it was configured: %v", err)
	}
}

func TestSetupEnginePickerEOFStopsBeforeConfirmationAndStages(t *testing.T) {
	path := testCLIConfig(t)
	var output bytes.Buffer
	err := setupForTest(t.Context(), setupOptions{
		home: recordHome(path), name: recordName(path), discovery: subscriptionDisplayRunner(t),
		local: defaultsRunner(func(context.Context, installer.Command) ([]byte, error) {
			return []byte("Status: running\n"), nil
		}),
	}, strings.NewReader(""), &output, &output, func(context.Context, []string, io.Writer, io.Writer) error {
		t.Fatal("missing engine selection reached installation stages")
		return nil
	})
	if !errors.Is(err, io.EOF) || strings.Contains(output.String(), "Continue with this billable installation?") {
		t.Fatalf("missing engine selection was silently accepted: %v\n%s", err, &output)
	}
}

func TestSetupPinsAutoDetectedEngineAcrossStages(t *testing.T) {
	path := testCLIConfig(t)
	var output bytes.Buffer
	var probes []string
	err := setupForTest(t.Context(), setupOptions{
		home: recordHome(path), name: recordName(path), yes: true, discovery: subscriptionDisplayRunner(t),
		local: defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
			probes = append(probes, c.Args[0])
			if c.Args[0] == "docker" {
				return nil, errors.New("Docker unavailable")
			}
			return nil, nil
		}),
	}, strings.NewReader(""), &output, &output, func(_ context.Context, args []string, _, _ io.Writer) error {
		if args[0] == "doctor" || args[0] == "images" {
			if !strings.Contains(strings.Join(args, " "), "--container-engine podman") {
				t.Fatalf("setup did not pin its disclosed engine: %v", args)
			}
		}
		return nil
	})
	if err != nil || strings.Join(probes, ",") != "docker,podman" {
		t.Fatalf("auto-detection did not select Podman once: %v, %v", probes, err)
	}
}

func TestSetupBuilderFailureStopsBeforeBillingConfirmation(t *testing.T) {
	for _, phase := range []string{"daemon", "selected builder"} {
		t.Run(phase, func(t *testing.T) {
			path := testCLIConfig(t)
			var output bytes.Buffer
			unavailable := errors.New("builder unavailable")
			err := setupForTest(t.Context(), setupOptions{
				home: recordHome(path), name: recordName(path), discovery: subscriptionDisplayRunner(t),
				local: defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
					if phase == "selected builder" {
						switch strings.Join(c.Args, " ") {
						case "docker info --format {{json .ServerVersion}}", "docker buildx version":
							return nil, nil
						}
					}
					return nil, unavailable
				}),
			}, strings.NewReader("yes\n"), &output, &output, func(context.Context, []string, io.Writer, io.Writer) error {
				t.Fatal("unavailable image builder reached installation stages")
				return nil
			})
			if !errors.Is(err, unavailable) || !strings.Contains(err.Error(), "no usable image builder") ||
				strings.Contains(output.String(), "Continue with this billable installation?") {
				t.Fatalf("unavailable builder was not rejected before billing confirmation: %v\n%s", err, &output)
			}
		})
	}
}

func TestSetupDeclinesBeforeCloudChanges(t *testing.T) {
	path := testCLIConfig(t)
	called := false
	var output bytes.Buffer
	err := setupForTest(t.Context(), setupOptions{home: recordHome(path), name: recordName(path), discovery: subscriptionDisplayRunner(t)}, strings.NewReader("\n"), &output, &output,
		func(context.Context, []string, io.Writer, io.Writer) error {
			called = true
			return nil
		})
	if err == nil || called || !strings.Contains(err.Error(), "before provisioning") {
		t.Fatalf("setup changed resources without consent: called=%v, err=%v", called, err)
	}
}

func TestSetupRejectsUnsupportedDatabaseBeforeAnyStage(t *testing.T) {
	for _, backend := range []installer.DatabaseBackend{installer.DatabaseFlexible, installer.DatabaseHorizon} {
		t.Run(string(backend), func(t *testing.T) {
			path := testCLIConfig(t)
			c, err := installer.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			c.Database = installer.DatabaseConfig{Backend: backend}
			data, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			runner := defaultsRunner(func(_ context.Context, command installer.Command) ([]byte, error) {
				if isRegionList(command) && strings.Contains(strings.Join(command.Args, " "), "/locations?") {
					return []byte(testRegionsJSON), nil
				}
				args := strings.Join(command.Args, " ")
				if strings.Contains(args, "group exists") {
					return []byte("false"), nil
				}
				if backend == installer.DatabaseFlexible && strings.Contains(args, "postgres flexible-server list-skus") {
					return []byte(`[{"reason":"Subscriptions are restricted from provisioning in this region."}]`), nil
				}
				if backend == installer.DatabaseHorizon && strings.Contains(args, "/capabilities?") {
					return []byte(`{"value":[]}`), nil
				}
				t.Fatalf("unexpected discovery command: %v", command.Args)
				return nil, nil
			})
			var output bytes.Buffer
			err = setupForTest(t.Context(), setupOptions{home: recordHome(path), name: recordName(path), yes: true, discovery: runner},
				strings.NewReader(""), &output, &output, func(context.Context, []string, io.Writer, io.Writer) error {
					t.Fatal("unsupported database reached a setup stage")
					return nil
				})
			if err == nil || !strings.Contains(err.Error(), "verify the original database settings") {
				t.Fatalf("setup accepted unsupported database region: %v", err)
			}
			saved, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(saved, data) {
				t.Fatal("failed region validation changed the saved configuration")
			}
		})
	}
}

func TestSetupRetriesRestrictedExistingDatabase(t *testing.T) {
	for _, backend := range []installer.DatabaseBackend{installer.DatabaseFlexible, installer.DatabaseHorizon} {
		t.Run(string(backend), func(t *testing.T) {
			path := testCLIConfig(t)
			c, err := installer.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			c.Database = installer.DatabaseConfig{Backend: backend, Location: "eastus"}
			data, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			kind, host := "Microsoft.DBforPostgreSQL/flexibleServers", "pgexample.postgres.database.azure.com"
			if backend == installer.DatabaseHorizon {
				kind, host = "Microsoft.HorizonDB/clusters", "pgexample.abcdef.eastus.horizondb.azure.com"
			}
			id := "/subscriptions/" + c.Azure.SubscriptionID + "/resourceGroups/" + c.Azure.ResourceGroup + "/providers/" + kind + "/pgexample"
			db := &installer.DatabaseInfrastructure{Backend: backend, Location: "eastus", ResourceID: id,
				Host: host, Name: "substrate", Schema: "substrate", EgressIP: "20.1.2.3"}
			outputs, err := json.Marshal(installer.Infrastructure{
				ClusterName: c.AKS.ClusterName, ResourceGroup: c.Azure.ResourceGroup,
				StorageAccount: "substratetest123", AccountURL: "https://substratetest123.blob.core.windows.net",
				SnapshotsContainer: "snapshots", AssetsContainer: "assets",
				NodeClientID: defaultPublisher, ControlClientID: defaultPublisher, TenantID: defaultPublisher, Database: db,
			})
			if err != nil {
				t.Fatal(err)
			}
			runner := defaultsRunner(func(_ context.Context, command installer.Command) ([]byte, error) {
				args := strings.Join(command.Args, " ")
				switch {
				case isRegionList(command) && strings.Contains(args, "/locations?"):
					return json.Marshal([]string{c.Azure.Location, c.DatabaseLocation()})
				case strings.Contains(args, "postgres flexible-server list-skus"):
					return []byte("[]"), nil
				case strings.Contains(args, "/capabilities?"):
					return []byte(`{"value":[]}`), nil
				case strings.Contains(args, "group exists"):
					return []byte("true"), nil
				case strings.Contains(args, "group show"):
					return json.Marshal(map[string]any{"location": c.Azure.Location, "tags": map[string]string{
						"substrate-aks-installation": c.Installation.ID, "substrate-aks-database": string(backend),
						"substrate-aks-database-location": c.DatabaseLocation(),
					}})
				case strings.Contains(args, "deployment sub list"):
					return outputs, nil
				case strings.Contains(args, "resource show --ids "+id):
					return json.Marshal(map[string]any{"id": id, "location": "eastus",
						"tags": map[string]string{"substrate-aks-installation": c.Installation.ID}})
				case strings.Contains(args, "account show"):
					return []byte(accountJSON("user")), nil
				default:
					t.Fatalf("unexpected discovery command: %v", command.Args)
					return nil, nil
				}
			})
			stopped := errors.New("stop at first setup stage")
			var output bytes.Buffer
			err = setupForTest(t.Context(), setupOptions{home: recordHome(path), name: recordName(path), yes: true, discovery: runner},
				strings.NewReader(""), &output, &output, func(_ context.Context, args []string, _, _ io.Writer) error {
					if args[0] != "plan" {
						t.Fatalf("unexpected first stage: %v", args)
					}
					return stopped
				})
			if !errors.Is(err, stopped) || !strings.Contains(output.String(), "Warning: Azure no longer advertises") {
				t.Fatalf("saved setup did not continue with a warning: %v; %s", err, output.String())
			}
			saved, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(saved, data) {
				t.Fatal("retry changed the saved configuration")
			}
		})
	}
}

func TestSetupOffersOnlySupportedDatabaseRegions(t *testing.T) {
	for _, backend := range []struct {
		name, region, capabilities string
	}{
		{"flexible-server", "westus3", `[{"supportedServerVersions":[{"name":"17"}],"supportedServerEditions":[{"name":"GeneralPurpose","supportedServerSkus":[{"name":"Standard_D2ads_v5"}]}]}]`},
		{"horizondb", "eastus", `{"value":[{"location":"eastus","clusterCapabilities":{"supportedPostgreSqlVersions":[{"name":"17"}]},"poolCapabilities":{"supportedProcessors":[{"name":"Intel","supportedGenerations":[{"computeGeneration":1,"supportedComputeModels":[{"type":"Provisioned","supportedComputeSizes":[{"vCores":2}]}]}]}]}}]}`},
	} {
		outcomes := []string{"select supported region", "EOF before selection", "cancel discovery"}
		for _, outcome := range outcomes {
			t.Run(backend.name+"/"+outcome, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				path := testRecord(t, "substrate-test")
				var mu sync.Mutex
				checked := map[string]int{}
				runner := defaultsRunner(func(_ context.Context, command installer.Command) ([]byte, error) {
					args := strings.Join(command.Args, " ")
					if strings.Contains(args, "provider show") {
						return []byte(`["South Central US","` + backend.region + `"]`), nil
					}
					if strings.Contains(args, "/locations?api-version=") {
						return []byte(`["eastus","southcentralus","westus3"]`), nil
					}
					if strings.HasPrefix(args, "az account show ") {
						return []byte(accountJSON("user")), nil
					}
					if args == "az ad signed-in-user show --only-show-errors --output json" {
						return []byte(`{"id":"` + defaultPublisher + `"}`), nil
					}
					if !strings.Contains(args, "postgres flexible-server list-skus") && !strings.Contains(args, "/capabilities?") {
						t.Fatalf("unexpected discovery command: %v", command.Args)
					}
					if strings.Contains(args, "--location southcentralus") || strings.Contains(args, "/locations/southcentralus/") {
						mu.Lock()
						checked["southcentralus"]++
						mu.Unlock()
						if outcome == "cancel discovery" {
							cancel()
							return nil, context.Canceled
						}
						if backend.name == "flexible-server" {
							return []byte(`[{"reason":"Subscriptions are restricted from provisioning in this region."}]`), nil
						}
						return []byte(`{"value":[]}`), nil
					}
					if !strings.Contains(args, "--location "+backend.region) &&
						!strings.Contains(args, "/locations/"+backend.region+"/") {
						t.Fatalf("capability request targeted an unexpected region: %v", command.Args)
					}
					mu.Lock()
					checked[backend.region]++
					mu.Unlock()
					return []byte(backend.capabilities), nil
				})
				answers := "\n\n\n"
				if outcome == "select supported region" {
					// Approve billing and decline the demo; the first stage stops setup
					// so the new installation is kept for inspection.
					answers += "southcentralus\n" + backend.region + "\nyes\nno\n"
				}
				var output bytes.Buffer
				err := setupForTest(ctx, setupOptions{
					home: recordHome(path), subscription: defaultSubscription, publisher: defaultPublisher, reader: defaultPublisher,
					cidr: "203.0.113.8/32", name: "substrate-test", location: "southcentralus",
					database: backend.name, discovery: runner, accessible: true,
				}, strings.NewReader(answers), &output, &output, func(ctx context.Context, args []string, out, stderr io.Writer) error {
					if outcome != "select supported region" {
						t.Fatal("incomplete region selection reached a setup stage")
					}
					return stopAtFirstStage(ctx, args, out, stderr)
				})
				if outcome == "select supported region" {
					if !errors.Is(err, errFirstStage) {
						t.Fatalf("setup did not recover and reach the first stage: %v\n%s", err, &output)
					}
					c, loadErr := installer.LoadConfig(path)
					if loadErr != nil || c.DatabaseLocation() != backend.region || c.Azure.Location != "southcentralus" {
						t.Fatalf("region selection was not saved independently of AKS: %+v, %v", c, loadErr)
					}
					if checked["southcentralus"] != 1 || checked[backend.region] != 2 ||
						!strings.Contains(output.String(), "Available database regions: "+backend.region) ||
						!strings.Contains(output.String(), "southcentralus is not available") ||
						!strings.Contains(output.String(), `unknown Azure region "southcentralus"`) {
						t.Fatalf("setup offered or accepted an unsupported region: %v\n%s", checked, &output)
					}
					// Discovery is slow, so it must run as a visible check before the picker.
					check := strings.Index(output.String(), "Database region availability")
					step := strings.Index(output.String(), "can take up to a minute")
					picker := strings.Index(output.String(), "Available database regions")
					if check < 0 || step < check || picker < step {
						t.Fatalf("region discovery was not presented as a timed check before the picker:\n%s", &output)
					}
				} else {
					want := io.EOF
					if outcome == "cancel discovery" {
						want = context.Canceled
					}
					if !errors.Is(err, want) {
						t.Fatalf("setup lost cancellation: %v, %v", err, checked)
					}
					if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("incomplete database selection saved a configuration")
					}
				}
			})
		}
	}
}

func TestSetupKeepsExistingConfigurationAuthoritative(t *testing.T) {
	for _, flags := range [][]string{
		{"--identity-object-id", defaultPublisher},
		{"--database", "horizondb"},
		{"--database-location", "eastus"},
	} {
		t.Run(flags[0], func(t *testing.T) {
			path := testCLIConfig(t)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", "")
			var output bytes.Buffer
			err = run(t.Context(), append([]string{"setup", recordName(path), "--yes"}, flags...), &output, &output)
			if err == nil || !strings.Contains(err.Error(), "only apply to new installations") {
				t.Fatalf("setup silently overrode configuration: %v", err)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
				t.Fatalf("rejected flags changed the saved installation: %v", err)
			}
		})
	}
}

func TestSetupDeclinesPlannedChanges(t *testing.T) {
	for _, tc := range []struct {
		name, answers, commands string
	}{
		{"registry preview", "yes\nno\nno\n", "plan"},
		{"full preview", "yes\nno\nyes\nno\n", "plan,provision,images,images,plan"},
		{"interrupted full preview", "yes\nno\nyes\n", "plan,provision,images,images,plan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := testCLIConfig(t)
			var commands []string
			var output bytes.Buffer
			err := setupForTest(t.Context(), setupOptions{home: recordHome(path), name: recordName(path), discovery: subscriptionDisplayRunner(t)}, strings.NewReader(tc.answers),
				&output, &output, func(_ context.Context, args []string, _, _ io.Writer) error {
					commands = append(commands, args[0])
					return nil
				})
			if err == nil || !strings.Contains(err.Error(), "Resources are retained") ||
				!strings.Contains(err.Error(), "Teardown:") {
				t.Fatalf("preview cancellation lost cleanup guidance: %v", err)
			}
			if strings.Join(commands, ",") != tc.commands {
				t.Fatalf("setup applied changes without plan approval: %v", commands)
			}
		})
	}
}

func TestCLICommandQuotesOnlyWhenNeeded(t *testing.T) {
	for _, tc := range []struct {
		name, executable string
		args             []string
		want             string
	}{
		{
			name: "ordinary destroy command", executable: "bin/substrate-aks",
			args: []string{"destroy", "substrate-20261028-201041", "--confirm-delete", "substrate-20261028-201041"},
			want: "bin/substrate-aks destroy substrate-20261028-201041 --confirm-delete substrate-20261028-201041",
		},
		{
			name: "executable and arguments with spaces", executable: "/tmp/substrate cli",
			args: []string{"setup", "operator's install", "--container-engine=podman"},
			want: `'/tmp/substrate cli' setup 'operator'"'"'s install' --container-engine=podman`,
		},
		{
			name: "shell syntax and empty arguments", executable: "bin/substrate-aks",
			args: []string{"setup", "$(printf unsafe);*", "--name", ""},
			want: "bin/substrate-aks setup '$(printf unsafe);*' --name ''",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := os.Args
			os.Args = []string{tc.executable}
			t.Cleanup(func() { os.Args = original })
			if got := cliCommand(tc.args...); got != tc.want {
				t.Fatalf("command = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestTeardownCommandNamesInstallationAndResourceGroup(t *testing.T) {
	original := os.Args
	os.Args = []string{"bin/substrate-aks"}
	t.Cleanup(func() { os.Args = original })
	want := "bin/substrate-aks destroy substrate-dev --confirm-delete substrate-dev-rg"
	if got := teardownCommand("substrate-dev", "substrate-dev-rg"); got != want {
		t.Fatalf("teardown command = %q; want %q", got, want)
	}
}

func TestClientHandoffQuotesPathsForTheShell(t *testing.T) {
	t.Setenv("PATH", "")
	var out bytes.Buffer
	if err := printClientSetup(&out, installer.ClientSetup{
		Kubeconfig: "/tmp/operator's cluster/kubeconfig", Context: "substrate-test", BinDir: "/tmp/client bin",
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`export KUBECONFIG='/tmp/operator'"'"'s cluster/kubeconfig'`, `export PATH='/tmp/client bin':"$PATH"`, "kubectl ate --help"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("handoff lost shell quoting or direct client: %q", out.String())
		}
	}
}

func TestClientHandoffOnlyPrintsPathWhenNeeded(t *testing.T) {
	bin, other := t.TempDir(), t.TempDir()
	fakeTool(t, bin, "kubectl-ate", "exit 0")
	fakeTool(t, other, "kubectl-ate", "exit 0")
	for _, tc := range []struct {
		name, path string
		export     bool
	}{
		{"already on PATH", bin, false},
		{"not on PATH", "", true},
		{"another plugin first", other + string(os.PathListSeparator) + bin, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tc.path)
			var out bytes.Buffer
			if err := printClientSetup(&out, installer.ClientSetup{
				Kubeconfig: "/tmp/kubeconfig", Context: "substrate-test", BinDir: bin,
			}); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "export PATH=") != tc.export ||
				!strings.Contains(out.String(), "export KUBECONFIG=") ||
				!strings.Contains(out.String(), "kubectl ate --help") {
				t.Fatalf("incorrect shell handoff: %s", &out)
			}
		})
	}
}

func TestSetupReusesConfiguredImagesOnRetry(t *testing.T) {
	path := testCLIConfig(t)
	c, err := installer.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Substrate.Images = installer.ImagesConfig{
		Repository: "example.invalid/substrate", Tag: "test",
		Plugin: "example.invalid/plugin@sha256:" + strings.Repeat("a", 64),
	}
	c.Substrate.Runtime.AssetsDir = testRuntimeAssets(t)
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	var commands []string
	var output bytes.Buffer
	err = setupForTest(t.Context(), setupOptions{
		home: recordHome(path), name: recordName(path), yes: true, discovery: subscriptionDisplayRunner(t),
		local: defaultsRunner(func(context.Context, installer.Command) ([]byte, error) {
			t.Fatal("configured image reuse probed a container engine")
			return nil, nil
		}),
	}, strings.NewReader(""), &output, &output,
		func(_ context.Context, args []string, _, _ io.Writer) error {
			commands = append(commands, args[0])
			if strings.Contains(strings.Join(args, " "), "--container-engine") {
				t.Fatal("prebuilt-image reuse required an image builder")
			}
			if args[0] == "images" && !strings.Contains(strings.Join(args, " "), "--check") {
				t.Fatal("configured image reuse attempted publication")
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(commands, ",") != "images,plan,provision,check,prepare,install,status,client" {
		t.Fatalf("retry rebuilt images or changed the requested demo selection: %v", commands)
	}
	if strings.Count(output.String(), "DONE  Setup complete.") != 1 ||
		strings.Contains(output.String(), "Counter microVM demo installed.") || strings.Contains(output.String(), "# Try the counter demo:") {
		t.Fatalf("setup without the demo omitted completion or falsely advertised a deployed demo:\n%s", &output)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatalf("valid image reuse changed saved configuration: %v", err)
	}
}

func TestSetupRejectsStaleImagesBeforeProvisioning(t *testing.T) {
	path := testCLIConfig(t)
	c, err := installer.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Substrate.Images = installer.ImagesConfig{
		Repository: "substratetest123.azurecr.io/substrate", Tag: "deleted-build",
		Plugin: "substratetest123.azurecr.io/plugin@sha256:" + strings.Repeat("a", 64),
	}
	c.Substrate.Runtime.AssetsDir = testRuntimeAssets(t)
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	missing := errors.New("configured image is missing from the recreated empty registry")
	var commands []string
	var output bytes.Buffer
	err = setupForTest(t.Context(), setupOptions{home: recordHome(path), name: recordName(path), yes: true, discovery: subscriptionDisplayRunner(t)},
		strings.NewReader(""), &output, &output, func(_ context.Context, args []string, _, _ io.Writer) error {
			commands = append(commands, args[0])
			if args[0] == "images" && strings.Contains(strings.Join(args, " "), "--check") {
				return missing
			}
			return nil
		})
	if !errors.Is(err, missing) {
		t.Fatalf("setup trusted saved references without checking publication: commands=%v, error=%v", commands, err)
	}
	if strings.Join(commands, ",") != "images" {
		t.Fatalf("stale images reached infrastructure or installation work: %v", commands)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatalf("setup silently changed the saved image set: %v", err)
	}
}

func TestSetupNamePromptRejectsSavedInstallations(t *testing.T) {
	path := testCLIConfig(t)
	home := recordHome(path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = setupForTest(t.Context(), setupOptions{
		home: home, subscription: defaultSubscription, publisher: defaultPublisher,
		cidr: "203.0.113.8/32", location: "eastus2", accessible: true, skipContextHandoff: true,
		discovery: subscriptionDisplayRunner(t), azure: successfulCloudRunnerFor(home, "fresh-install"),
	}, strings.NewReader("substrate-test\nfresh-install\n\n\n\nno\n"), &output, &output,
		func(context.Context, []string, io.Writer, io.Writer) error {
			t.Fatal("declined setup ran a stage")
			return nil
		})
	if err == nil || !strings.Contains(output.String(), "installation substrate-test already exists; run setup substrate-test to resume it") {
		t.Fatalf("taken name was not rejected: %v\n%s", err, &output)
	}
	if !strings.Contains(strings.Join(strings.Fields(output.String()), ""), "installations/fresh-install/installation.json") {
		t.Fatalf("setup did not review the replacement name:\n%s", &output)
	}
	// Declining billing discards the installation this run created.
	if _, err := os.Stat(filepath.Join(home, "installations", "fresh-install")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("declined setup left the new installation behind: %v", err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("setup changed the existing installation: %v", err)
	}
}
