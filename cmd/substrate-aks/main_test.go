package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/substrate-aks/internal/installer"
)

func TestReportCommandErrorDistinguishesIntentionalCancellation(t *testing.T) {
	recovery := fmt.Errorf("setup stopped at infrastructure preview: %w\n\nResources are retained.\nRetry failed stage: substrate-aks plan --config deploy.json\nTeardown: substrate-aks destroy --config deploy.json --confirm-delete test-cluster", context.Canceled)
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
	dir := t.TempDir()
	t.Chdir(dir)
	bin := t.TempDir()
	fakeTool(t, bin, "az", `test "$1" = rest || exit 19; printf '%s' '`+testRegionsJSON+`'`)
	t.Setenv("PATH", bin)
	path := filepath.Join(dir, "deploy.json")
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

func TestInitUsesAzureContextDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deploy.json")
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
	if err := run(t.Context(), []string{"init", "--config", path, "--api-cidr", "203.0.113.8/32"}, &out, &out); err != nil {
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
	path := filepath.Join(t.TempDir(), "deploy.json")
	var out bytes.Buffer
	args := []string{"init", "--config", path, "--subscription", defaultSubscription,
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
	if err := run(t.Context(), []string{"images", "--reset-config", "--config", path}, &out, &out); err != nil {
		t.Fatal(err)
	}
	reset, err := installer.LoadConfig(path)
	if err != nil || reset.Substrate.Images != (installer.ImagesConfig{}) ||
		reset.Installation.ID != c.Installation.ID || !strings.Contains(out.String(), "Cleared saved image references") {
		t.Fatalf("reset did not provide a usable setup recovery path: %+v, %v, %s", reset.Substrate.Images, err, &out)
	}
}
