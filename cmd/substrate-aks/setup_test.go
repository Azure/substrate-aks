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
	"strings"
	"testing"

	"github.com/Azure/substrate-aks/internal/installer"
)

func subscriptionDisplayRunner(t *testing.T) installer.Runner {
	t.Helper()
	return defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
		if isRegionList(c) {
			return []byte(testRegionsJSON), nil
		}
		want := "az account show --only-show-errors --output json --subscription " + defaultSubscription
		if strings.Join(c.Args, " ") != want {
			t.Fatalf("subscription display consulted the wrong context: %v", c.Args)
		}
		return []byte(accountJSON("user")), nil
	})
}

func setupForTest(ctx context.Context, o setupOptions, in io.Reader, out, stderr io.Writer, execute commandRunner) error {
	if o.contextFile == "" {
		o.contextFile = filepath.Join(filepath.Dir(o.path), "operator-kubeconfig")
	}
	if o.subscription != "" && o.publisher != "" && o.operatorID == "" {
		o.operatorID = defaultPublisher
	}
	if o.local == nil {
		o.local = defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
			switch strings.Join(c.Args, " ") {
			case "docker info --format {{json .ServerVersion}}", "docker buildx version":
				return nil, nil
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
	return setup(ctx, o, in, out, stderr, execute)
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
	for _, fail := range []string{"", "images"} {
		t.Run("failure="+fail, func(t *testing.T) {
			path := testCLIConfig(t)
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
				if len(args) < 3 || args[1] != "--config" || args[2] != path {
					t.Fatalf("stage lost config: %v", args)
				}
				if args[0] == fail {
					return want
				}
				return nil
			}
			var output bytes.Buffer
			err := setupForTest(t.Context(), setupOptions{path: path, yes: true, withDemo: true, discovery: subscriptionDisplayRunner(t)},
				strings.NewReader(""), &output, &output, execute)
			if fail != "" {
				if !errors.Is(err, want) || !strings.Contains(err.Error(), "Resources are retained") {
					t.Fatalf("stage failure lost retry/cleanup guidance: %v", err)
				}
				if strings.Join(commands, ",") != "plan,provision,images" {
					t.Fatalf("setup continued after failure: %v", commands)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if strings.Join(commands, ",") != "plan,provision,images,images,plan,provision,check,prepare,install,status,demo,client" {
					t.Fatalf("setup lost a required stage or order: %v", commands)
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
	err := setupForTest(t.Context(), setupOptions{path: path, yes: true, containerEngine: "podman", discovery: subscriptionDisplayRunner(t)},
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
	err := run(t.Context(), []string{"setup", "--container-engine", "unknown", "--config", "/missing-config"}, &output, &output)
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
				path: path, containerEngine: tc.requested, discovery: subscriptionDisplayRunner(t),
				local: defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
					if c.Args[0] != tc.available {
						return nil, errors.New("builder unavailable in this fixture")
					}
					return nil, nil
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
				path: path, discovery: subscriptionDisplayRunner(t),
				local: defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
					probes = append(probes, strings.Join(c.Args, " "))
					return nil, nil
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
				strings.Join(probes, ",") != "docker info --format {{json .ServerVersion}},docker buildx version,podman info --format json" {
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
				path: path, yes: yes, containerEngine: requested, discovery: subscriptionDisplayRunner(t),
				local: defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
					probes = append(probes, c.Args[0])
					return nil, nil
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

func TestSetupEnginePickerEOFStopsBeforeConfirmationAndStages(t *testing.T) {
	path := testCLIConfig(t)
	var output bytes.Buffer
	err := setupForTest(t.Context(), setupOptions{
		path: path, discovery: subscriptionDisplayRunner(t),
		local: defaultsRunner(func(context.Context, installer.Command) ([]byte, error) { return nil, nil }),
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
		path: path, yes: true, discovery: subscriptionDisplayRunner(t),
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
	path := testCLIConfig(t)
	var output bytes.Buffer
	err := setupForTest(t.Context(), setupOptions{
		path: path, discovery: subscriptionDisplayRunner(t),
		local: defaultsRunner(func(context.Context, installer.Command) ([]byte, error) {
			return nil, errors.New("builder unavailable")
		}),
	}, strings.NewReader("yes\n"), &output, &output, func(context.Context, []string, io.Writer, io.Writer) error {
		t.Fatal("unavailable image builder reached installation stages")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "no usable image builder") ||
		strings.Contains(output.String(), "Continue with this billable installation?") {
		t.Fatalf("unavailable builder was not rejected before billing confirmation: %v\n%s", err, &output)
	}
}

func TestSetupDeclinesBeforeCloudChanges(t *testing.T) {
	path := testCLIConfig(t)
	called := false
	var output bytes.Buffer
	err := setupForTest(t.Context(), setupOptions{path: path, discovery: subscriptionDisplayRunner(t)}, strings.NewReader("\n"), &output, &output,
		func(context.Context, []string, io.Writer, io.Writer) error {
			called = true
			return nil
		})
	if err == nil || called || !strings.Contains(err.Error(), "before provisioning") {
		t.Fatalf("setup changed resources without consent: called=%v, err=%v", called, err)
	}
}

func TestSetupKeepsExistingConfigurationAuthoritative(t *testing.T) {
	path := testCLIConfig(t)
	t.Setenv("PATH", "")
	var output bytes.Buffer
	err := run(t.Context(), []string{"setup", "--config", path, "--yes",
		"--identity-object-id", defaultPublisher}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "edit the existing") {
		t.Fatalf("setup silently overrode configuration: %v", err)
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
			err := setupForTest(t.Context(), setupOptions{path: path, discovery: subscriptionDisplayRunner(t)}, strings.NewReader(tc.answers),
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
		path: path, yes: true, discovery: subscriptionDisplayRunner(t),
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
	err = setupForTest(t.Context(), setupOptions{path: path, yes: true, discovery: subscriptionDisplayRunner(t)},
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
