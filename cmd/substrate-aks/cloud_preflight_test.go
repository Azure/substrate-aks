package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/Azure/substrate-aks/internal/installer"
)

// successfulCloudRunner answers for the installation saved in home, resolved
// on each call because setup may create it after the runner is built.
func successfulCloudRunner(home string) installer.Runner {
	return successfulCloudRunnerFor(home, "")
}

func successfulCloudRunnerFor(home, name string) installer.Runner {
	return defaultsRunner(func(_ context.Context, command installer.Command) ([]byte, error) {
		name, err := installer.ResolveInstallation(home, name)
		if err != nil {
			return nil, err
		}
		c, err := installer.LoadInstallation(home, name)
		if err != nil {
			return nil, err
		}
		args := strings.Join(command.Args, " ")
		switch {
		case strings.HasPrefix(args, "az account show "):
			return []byte(fmt.Sprintf(`{"id":%q}`, c.Azure.SubscriptionID)), nil
		case strings.HasPrefix(args, "az group list "):
			return []byte(`[]`), nil
		case strings.HasPrefix(args, "az group exists "):
			return []byte(`false`), nil
		case strings.Contains(args, "/providers/Microsoft.Compute/skus?"):
			return []byte(`{"value":[{"name":"Standard_D4s_v6","family":"StandardDsv6Family","capabilities":[{"name":"vCPUs","value":"4"}]}]}`), nil
		case strings.HasPrefix(args, "az rest --method get "):
			return []byte(`{"value":[{"actions":["*"],"notActions":[]}]}`), nil
		case strings.HasPrefix(args, "az aks get-versions "):
			return []byte(`{"values":[{"version":"1.37","patchVersions":{"1.37.0":{}}}]}`), nil
		case strings.HasPrefix(args, "az vm list-usage "):
			return []byte(`[{"name":{"value":"cores"},"currentValue":"0","limit":"100"},{"name":{"value":"StandardDsv6Family"},"currentValue":"0","limit":"100"}]`), nil
		default:
			return nil, fmt.Errorf("unexpected cloud preflight command: %s", args)
		}
	})
}

func TestSetupCloudFailureStopsBeforeBillingAndDeployment(t *testing.T) {
	want := errors.New("subscription permissions cannot be verified")
	var output bytes.Buffer
	err := setupForTest(t.Context(), setupOptions{
		home: recordHome(testCLIConfig(t)), name: "substrate-test", yes: true, discovery: subscriptionDisplayRunner(t),
		azure: defaultsRunner(func(context.Context, installer.Command) ([]byte, error) { return nil, want }),
	}, strings.NewReader(""), &output, &output, func(context.Context, []string, io.Writer, io.Writer) error {
		t.Fatal("cloud preflight failure executed an installation stage")
		return nil
	})
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "before provisioning") ||
		strings.Contains(output.String(), "Continue with this billable installation?") {
		t.Fatalf("cloud failure lost the pre-billing boundary: %v\n%s", err, &output)
	}
}

func TestOperatorIdentityWarning(t *testing.T) {
	for _, tc := range []struct{ name, kind, current, want string }{
		{"matching", "User", defaultPublisher, ""},
		{"different", "User", "44444444-4444-4444-8444-444444444444", "differs from aks.operator"},
		{"group", "Group", "", "belongs to that group"},
		{"directory restricted", "User", "", "Could not verify"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := installer.LoadConfig(testCLIConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			c.AKS.Operator = &installer.OperatorConfig{ObjectID: defaultPublisher, PrincipalType: tc.kind}
			d := azureDefaults{signedInObjectID: tc.current, signedInType: "User"}
			var output bytes.Buffer
			r := defaultsRunner(func(context.Context, installer.Command) ([]byte, error) {
				if tc.name != "directory restricted" {
					t.Fatal("known identity or group should not trigger directory discovery")
				}
				return nil, errors.New("directory access denied")
			})
			if err := warnOperatorIdentity(t.Context(), c, d, r, newSetupUI(&output)); err != nil {
				t.Fatal(err)
			}
			if tc.want == "" && output.Len() != 0 || tc.want != "" && !strings.Contains(output.String(), tc.want) {
				t.Fatalf("identity warning: %s", &output)
			}
		})
	}
}

func TestCloudDoctorRequiresInstallationName(t *testing.T) {
	var output bytes.Buffer
	err := run(t.Context(), []string{"doctor", "--azure"}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "requires an installation NAME") {
		t.Fatalf("cloud doctor did not require an installation: %v", err)
	}
}

func TestSetupSavesDiscoveredPublisherTypeWithSeparateOperator(t *testing.T) {
	path := testRecord(t, "substrate-test")
	runner := defaultsRunner(func(_ context.Context, command installer.Command) ([]byte, error) {
		if isRegionList(command) {
			return []byte(testRegionsJSON), nil
		}
		if command.Args[1] == "account" {
			return []byte(accountJSON("servicePrincipal")), nil
		}
		return []byte(fmt.Sprintf(`{"id":%q}`, defaultPublisher)), nil
	})
	var output bytes.Buffer
	err := setupForTest(t.Context(), setupOptions{
		home: recordHome(path), name: recordName(path), subscription: defaultSubscription, operatorID: defaultClientID, operatorType: "Group",
		location: "eastus2", cidr: "203.0.113.8/32", discovery: runner,
	}, strings.NewReader("\n\nno\nyes\nno\n"), &output, &output, stopAtFirstStage)
	if !errors.Is(err, errFirstStage) {
		t.Fatalf("setup did not reach billing confirmation: %v\n%s", err, &output)
	}
	c, err := installer.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Azure.AssetPublisherPrincipalType != "ServicePrincipal" || c.AKS.Operator.PrincipalType != "Group" {
		t.Fatalf("publisher incorrectly inherited the separate operator's type: %+v", c)
	}
}
