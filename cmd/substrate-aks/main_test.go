package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Azure/substrate-aks/internal/installer"
)

// TestMain keeps every test away from the operator's real installation store;
// tests that need records set their own isolated home with testRecord.
func TestMain(m *testing.M) {
	const inherited = "SUBSTRATE_AKS_TEST_HOME"
	// Re-executed test binaries (signal tests) share the parent's home, which
	// the parent removes; they may be killed before running cleanup.
	if home := os.Getenv(inherited); home != "" {
		os.Setenv(installer.HomeEnv, home)
		os.Exit(m.Run())
	}
	home, err := os.MkdirTemp("", "substrate-aks-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv(installer.HomeEnv, home)
	os.Setenv(inherited, home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

func TestReportCommandErrorDistinguishesIntentionalCancellation(t *testing.T) {
	recovery := fmt.Errorf("setup stopped at infrastructure preview: %w\n\nResources are retained.\nRetry failed stage: substrate-aks plan substrate-dev\nTeardown: substrate-aks destroy substrate-dev --confirm-delete test-cluster", context.Canceled)
	for _, tc := range []struct {
		name string
		err  error
		want []string
		omit []string
	}{
		{"cancelled", context.Canceled, []string{"Stopped by user.\n"}, []string{"WARN", "ERROR", "context canceled"}},
		{"cancelled with recovery", recovery, []string{"Stopped by user.", recovery.Error()}, []string{"WARN", "ERROR"}},
		{"failed", errors.New("deployment failed"), []string{"ERROR", "deployment failed"}, []string{"Stopped by user."}},
		{"timed out", context.DeadlineExceeded, []string{"ERROR", "context deadline exceeded"}, []string{"Stopped by user."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			reportCommandError(&out, tc.err)
			for _, text := range tc.want {
				if !strings.Contains(out.String(), text) {
					t.Fatalf("missing %q in command error:\n%s", text, &out)
				}
			}
			for _, text := range tc.omit {
				if strings.Contains(out.String(), text) {
					t.Fatalf("unexpected %q in command error:\n%s", text, &out)
				}
			}
		})
	}
}

func TestInitDoesNotOverwriteAndCreatesValidConfig(t *testing.T) {
	path := testRecord(t, "substrate-dev")
	bin := t.TempDir()
	fakeTool(t, bin, "az", `test "$1" = rest || exit 19; printf '%s' '`+testRegionsJSON+`'`)
	t.Setenv("PATH", bin)
	args := []string{"init",
		"--subscription", "22222222-2222-4222-8222-222222222222",
		"--asset-publisher-object-id", "33333333-3333-4333-8333-333333333333",
		"--operator-object-id", defaultPublisher,
		"--api-cidr", "203.0.113.8/32"}
	var out bytes.Buffer
	if err := run(t.Context(), args, &out, &out); err != nil {
		t.Fatal(err)
	}
	c, err := installer.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Substrate.Source.Commit != installer.DefaultCommit {
		t.Fatal("init did not select the experimental pin")
	}
	if c.AKS.Operator == nil || c.AKS.Operator.ObjectID != defaultPublisher || c.AKS.Operator.PrincipalType != "User" {
		t.Fatal("new configuration did not enable the explicitly selected Entra operator")
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"init"}, &out, &out); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("init overwrote an existing configuration or tried discovery first: %v", err)
	}
	last, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, last) {
		t.Fatal("existing configuration changed")
	}
}

func TestInitSavesDatabaseInInitialRecord(t *testing.T) {
	path := testRecord(t, "flexible")
	bin := t.TempDir()
	fakeTool(t, bin, "az", `
case "$*" in
  *"flexible-server list-skus --location southcentralus"*) printf '%s' '[{"supportedServerVersions":[{"name":"17"}],"supportedServerEditions":[{"name":"GeneralPurpose","supportedServerSkus":[{"name":"Standard_D2ads_v5"}]}]}]';;
  "rest --method"*) printf '%s' '`+testRegionsJSON+`';;
  *) printf 'unexpected Azure operation: %s\n' "$*" >&2; exit 19;;
esac
`)
	t.Setenv("PATH", bin)
	var out bytes.Buffer
	if err := run(t.Context(), []string{"init", recordName(path), "--subscription", defaultSubscription,
		"--identity-object-id", defaultPublisher, "--operator-object-id", defaultPublisher,
		"--api-cidr", "203.0.113.8/32", "--database", "flexible-server", "--database-location", "southcentralus"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	c, err := installer.LoadInstallation(recordHome(path), recordName(path))
	if err != nil || c.Database != (installer.DatabaseConfig{Backend: installer.DatabaseFlexible, Location: "southcentralus"}) {
		t.Fatalf("database selection was not saved with the new installation: %+v, %v", c.Database, err)
	}
}

func TestNewManagedConfigRejectsUnsupportedRegion(t *testing.T) {
	bin := t.TempDir()
	for _, tool := range []string{"git", "go", "kubelogin"} {
		fakeTool(t, bin, tool, `test "$1" = version || test "$1" = --version || exit 19`)
	}
	fakeTool(t, bin, "kubectl", `printf '%s' '{"clientVersion":{"major":"1","minor":"37"}}'`)
	// New setups probe the image builder before Azure discovery.
	fakeTool(t, bin, "docker", `
case "$*" in
  "buildx inspect --bootstrap") echo "Status: running";;
  "info --format {{json .ServerVersion}}"|"buildx version") exit 0;;
  *) exit 19;;
esac
`)
	fakeTool(t, bin, "podman", `exit 19`)
	fakeTool(t, bin, "az", `
case "$*" in
  version|"bicep version") exit 0;;
  *flexible-server*) printf '%s' '[{"reason":"Subscriptions are restricted from provisioning in this region."}]';;
  "rest --method"*) printf '%s' '`+testRegionsJSON+`';;
  *) printf 'unexpected Azure operation\n' >&2; exit 19;;
esac
`)
	t.Setenv("PATH", bin)
	for _, command := range []string{"init", "setup"} {
		for _, backend := range []string{"flexible-server"} {
			t.Run(command+"/"+backend, func(t *testing.T) {
				path := testRecord(t, "substrate-test")
				args := []string{command, recordName(path), "--subscription", defaultSubscription,
					"--identity-object-id", defaultPublisher, "--operator-object-id", defaultPublisher,
					"--api-cidr", "203.0.113.8/32", "--database", backend}
				if command == "setup" {
					args = append(args, "--yes")
				}
				var output bytes.Buffer
				err := run(t.Context(), args, &output, &output)
				if err == nil || !strings.Contains(err.Error(), "no alternate region") {
					t.Fatalf("unsupported region was accepted: %v", err)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("unsupported region created an installation record")
				}
			})
		}
	}
}

func TestInitUsesAzureContextDefaults(t *testing.T) {
	path := testRecord(t, "substrate-test")
	bin := t.TempDir()
	fakeTool(t, bin, "az", `
case "$1 $2" in
  "rest --method") printf '%s' '`+testRegionsJSON+`';;
  "account show") printf '%s' '`+accountJSON("user")+`';;
  "ad signed-in-user") printf '%s' '{"id":"`+defaultPublisher+`","displayName":"Publisher"}';;
  *) printf 'unexpected Azure operation\n' >&2; exit 19;;
esac
`)
	t.Setenv("PATH", bin)
	var out bytes.Buffer
	if err := run(t.Context(), []string{"init", recordName(path), "--api-cidr", "203.0.113.8/32"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	c, err := installer.LoadConfig(path)
	if err != nil || c.Azure.SubscriptionID != defaultSubscription || c.Azure.AssetPublisherObjectID != defaultPublisher ||
		c.AKS.Operator == nil || c.AKS.Operator.ObjectID != defaultPublisher || c.AKS.Operator.PrincipalType != "User" {
		t.Fatalf("init did not save Azure context defaults: %+v, %v", c.Azure, err)
	}
	if !strings.Contains(out.String(), "Development ("+defaultSubscription+")") ||
		!strings.Contains(out.String(), "Publisher ("+defaultPublisher+")") {
		t.Fatalf("init did not display the resolved target: %s", &out)
	}
}

func TestInitAllowsUnrestrictedAPIOnlyExplicitly(t *testing.T) {
	bin := t.TempDir()
	fakeTool(t, bin, "az", `test "$1" = rest || exit 19; printf '%s' '`+testRegionsJSON+`'`)
	t.Setenv("PATH", bin)
	path := testRecord(t, "substrate-test")
	var out bytes.Buffer
	args := []string{"init", recordName(path), "--subscription", defaultSubscription,
		"--identity-object-id", defaultPublisher, "--operator-object-id", defaultPublisher, "--allow-unrestricted-api"}
	if err := run(t.Context(), args, &out, &out); err != nil {
		t.Fatal(err)
	}
	c, err := installer.LoadConfig(path)
	if err != nil || !c.AKS.AllowUnrestrictedAPI || len(c.AKS.APIAuthorizedCIDRs) != 0 {
		t.Fatalf("explicit unrestricted flag did not survive configuration creation: %+v, %v", c.AKS, err)
	}
	if !strings.Contains(out.String(), "WARNING: "+unrestrictedAPIWarning) {
		t.Fatalf("unrestricted access was not warned about: %s", &out)
	}
	for _, command := range []string{"init", "setup"} {
		err := run(t.Context(), []string{command, "--api-cidr", "203.0.113.8/32", "--allow-unrestricted-api"}, &out, &out)
		if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("conflicting API flags were not rejected before discovery: %v", err)
		}
	}
}

func TestCLIRejectsUnknownCommandsAndUnexpectedFlags(t *testing.T) {
	for _, args := range [][]string{
		{"unknown"}, {"provision", "--component", "atelet"}, {"destroy", "--confirm-delete"},
		{"status", "unexpected"}, {"images", "--registry-only"}, {"install", "--registry-only"},
		{"actor", "--", "get", "actors"},
		{"images", "--check", "--reset-config"}, {"install", "--reset-config"},
		{"client", "--skip-context-handoff"},
	} {
		var out bytes.Buffer
		if err := run(t.Context(), args, &out, &out); err == nil {
			t.Fatalf("accepted invalid invocation: %v", args)
		}
	}
	var out bytes.Buffer
	if err := run(t.Context(), []string{"--help"}, &out, &out); err != nil || !strings.Contains(out.String(), "destroy") {
		t.Fatalf("help failed without configuration or Azure access: %v", err)
	}
}

func TestSetupRejectsConflictingContextHandoffFlags(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var out bytes.Buffer
	err := run(t.Context(), []string{"setup", "--yes", "--skip-context-handoff", "--context-file", "config"}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "--skip-context-handoff and --context-file are mutually exclusive") {
		t.Fatalf("conflicting handoff flags were not rejected before tool checks: %v", err)
	}
}

func TestCLIImageResetDoesNotBuildOrDeleteResources(t *testing.T) {
	path := testCLIConfig(t)
	c, err := installer.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	c.Substrate.Images = installer.ImagesConfig{
		Repository: "example.invalid/substrate", Tag: "deleted-build",
		Plugin: "example.invalid/plugin@sha256:" + strings.Repeat("a", 64),
	}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	fakeTool(t, bin, "az", `
case "$1 $2" in
  "account show") printf '%s' '{"id":"`+c.Azure.SubscriptionID+`"}';;
  "group list") printf '[]';;
  "group exists") printf false;;
  *) printf 'unexpected Azure operation\n' >&2; exit 19;;
esac
`)
	t.Setenv("PATH", bin)
	var out bytes.Buffer
	if err := run(t.Context(), []string{"images", "--reset-config", recordName(path)}, &out, &out); err != nil {
		t.Fatal(err)
	}
	reset, err := installer.LoadConfig(path)
	if err != nil || reset.Substrate.Images != (installer.ImagesConfig{}) ||
		reset.Installation.ID != c.Installation.ID || !strings.Contains(out.String(), "Cleared saved image references") {
		t.Fatalf("reset did not provide a usable setup recovery path: %+v, %v, %s", reset.Substrate.Images, err, &out)
	}
}

func TestCLIImageVerificationReportsWorkspaceSteps(t *testing.T) {
	for _, status := range []string{"available", "missing"} {
		t.Run(status, func(t *testing.T) {
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
			bin := t.TempDir()
			fakeTool(t, bin, "git", `
case "$1" in
  init|fetch|checkout) ;;
  rev-parse)
    /bin/mkdir -p manifests/ate-install cmd/ate-setup/internal/cmd cmd/ate-setup/internal/steps
    printf 'kind: SandboxConfig' > manifests/ate-install/sandboxconfig-microvm.yaml.tmpl
    printf '%s' '`+c.Substrate.Source.Commit+`';;
  *) exit 19;;
esac
`)
			image := ""
			if status == "missing" {
				image = c.Substrate.Images.Repository + "/missing:test"
			}
			fakeTool(t, bin, "go", `printf '%s' '`+fmt.Sprintf(
				`{"status":%q,"image":%q,"checked":18,"expected":18}`, status, image)+`'`)
			t.Setenv("PATH", bin)
			m := testWorkspace(t)
			m.welcome = false
			m.session.events = make(chan any, 1024)
			ctx := context.WithValue(t.Context(), setupSessionKey{}, m.session)
			ui := newSetupUI(&bytes.Buffer{})
			ui.session = m.session
			items := setupStageChecks("Verify configured images", "images", []string{"--check"})
			ui.checklist("Images and assets", items...)
			err = ui.checkGroup(items, func() error {
				return run(ctx, []string{"images", "--check", recordName(path)}, &bytes.Buffer{}, &bytes.Buffer{})
			})
			if (err != nil) != (status == "missing") {
				t.Fatalf("image verification returned %v for %s", err, status)
			}
			for len(m.session.events) > 0 {
				m.Update(<-m.session.events)
				if len(m.checks) != len(items) {
					t.Fatal("image verification changed its preset checklist")
				}
			}
			for i, check := range m.checks {
				failed := status == "missing" && i == len(items)-1
				if check.name != items[i] || !check.done || (check.err != nil) != failed {
					t.Fatalf("command result did not reach its checklist row: %+v", m.checks)
				}
			}
		})
	}
}

func TestInstallationNameArguments(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		args       []string
	}{
		{"name before flags", "installation not found: missing", []string{"status", "missing", "--verbose"}},
		{"name after flags", "installation not found: missing", []string{"status", "--verbose", "missing"}},
		{"name flag", "installation not found: missing", []string{"status", "--name", "missing"}},
		{"flags around name", "unknown container engine", []string{"doctor", "--azure", "missing", "--container-engine", "bogus"}},
		{"conflicting names", "given twice", []string{"status", "one", "--name", "two"}},
		{"invalid name", `installation name "Bad_Name"`, []string{"status", "Bad_Name"}},
		{"extra arguments", "unexpected arguments", []string{"status", "one", "two"}},
		{"destroy needs a name", "destroy requires the installation NAME", []string{"destroy", "--confirm-delete", "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testRecord(t, "substrate-test")
			var out bytes.Buffer
			if err := run(t.Context(), tc.args, &out, &out); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run(%q) = %v; want error containing %q", tc.args, err, tc.want)
			}
		})
	}
}

func TestCommandsResolveTheOnlyInstallationOrRequireAChoice(t *testing.T) {
	path := testCLIConfig(t)
	home := recordHome(path)
	c, err := installer.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if name, err := installer.ResolveInstallation(home, ""); err != nil || name != c.Installation.Name {
		t.Fatalf("single installation was not the default: %q, %v", name, err)
	}
	var out bytes.Buffer
	if err := initialize(home, c.Azure.SubscriptionID, c.Azure.AssetPublisherObjectID, "", "203.0.113.8/32",
		"substrate-other", "eastus2", false, nil, "", "", installer.DatabaseConfig{}, &out); err != nil {
		t.Fatal(err)
	}
	err = run(t.Context(), []string{"status"}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "substrate-other, substrate-test") {
		t.Fatalf("ambiguous command did not list the choices: %v", err)
	}
	out.Reset()
	if err := run(t.Context(), []string{"list"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "NAME") ||
		!strings.HasPrefix(lines[1], "substrate-other") || !strings.HasPrefix(lines[2], "substrate-test") ||
		!strings.Contains(lines[2], c.Azure.ResourceGroup) || !strings.Contains(lines[2], c.Azure.Location) {
		t.Fatalf("list did not show each installation's resource group and region:\n%s", &out)
	}
}

func TestDestroyKeepsLocalStateWhenManagedDatabaseBlocksDeletion(t *testing.T) {
	path := testCLIConfig(t)
	c, err := installer.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Database = installer.DatabaseConfig{Backend: installer.DatabaseFlexible, Location: "westus3"}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	fakeTool(t, bin, "az", fmt.Sprintf(`case "$1 $2" in
"account show") printf '{"id":"%s"}' ;;
"group exists") printf true ;;
"group show") printf '{"location":"%s","tags":{"substrate-aks-installation":"%s"}}' ;;
"group delete") echo "ResourceGroupDeletionBlocked: PostgreSQL server deletion is in progress" >&2; exit 1 ;;
*) exit 7 ;;
esac
`, c.Azure.SubscriptionID, c.Azure.Location, c.Installation.ID))
	t.Setenv("PATH", bin)
	var out bytes.Buffer
	err = run(t.Context(), []string{"destroy", c.Installation.Name, "--confirm-delete", c.Azure.ResourceGroup}, &out, &out)
	if err == nil {
		t.Fatal("blocked group deletion was reported as success")
	}
	if saved, err := os.ReadFile(path); err != nil || !bytes.Equal(saved, data) {
		t.Fatalf("failed teardown removed or changed the local installation needed for retry: %v", err)
	}
}

func TestDestroyRemovesLocalStateOnlyAfterAzureTeardown(t *testing.T) {
	path := testCLIConfig(t)
	c, err := installer.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	fakeTool(t, bin, "az", fmt.Sprintf(`case "$1 $2" in
"account show") printf '{"id":"%s"}' ;;
"group exists") printf false ;;
"group list") printf '[]' ;;
*) exit 7 ;;
esac
`, c.Azure.SubscriptionID))
	t.Setenv("PATH", bin)
	var out bytes.Buffer
	if err := run(t.Context(), []string{"destroy", c.Installation.Name, "--confirm-delete", "wrong"}, &out, &out); err == nil {
		t.Fatal("destroy accepted the wrong confirmation")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("failed destroy removed local state: %v", err)
	}
	if err := run(t.Context(), []string{"destroy", c.Installation.Name, "--confirm-delete", c.Azure.ResourceGroup}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.Installation.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destroy kept local installation state: %v", err)
	}
}

func TestDestroyKeepsLocalStateWhenAzureTeardownFails(t *testing.T) {
	path := testCLIConfig(t)
	c, err := installer.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	owned := fmt.Sprintf(`{"name":"%s","tags":{"substrate-aks-installation":"%s"},"location":"%s"}`, c.Azure.ResourceGroup, c.Installation.ID, c.Azure.Location)
	for _, tc := range []struct{ name, groups, want string }{
		{"retargeted installation", fmt.Sprintf(`[{"name":"other-rg","tags":{"substrate-aks-installation":"%s"}}]`, c.Installation.ID), "already owns resource group"},
		{"group delete failed", "[" + owned + "]", "group delete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			fakeTool(t, bin, "az", fmt.Sprintf(`case "$1 $2" in
"account show") printf '{"id":"%s"}' ;;
"group list") printf '%%s' '%s' ;;
"group exists") printf true ;;
"group show") printf '%%s' '%s' ;;
"group delete") echo "group delete failed" >&2; exit 7 ;;
*) exit 9 ;;
esac
`, c.Azure.SubscriptionID, tc.groups, owned))
			t.Setenv("PATH", bin)
			var out bytes.Buffer
			err := run(t.Context(), []string{"destroy", c.Installation.Name, "--confirm-delete", c.Azure.ResourceGroup}, &out, &out)
			if err == nil || !strings.Contains(err.Error()+out.String(), tc.want) {
				t.Fatalf("destroy error = %v, want %q:\n%s", err, tc.want, &out)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("failed destroy removed local state: %v", err)
			}
		})
	}
}
