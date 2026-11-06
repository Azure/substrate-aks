package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/substrate-aks/internal/installer"
)

func TestLocalFailurePrecedesAzureDiscovery(t *testing.T) {
	missing := errors.New("missing Azure CLI")
	var output bytes.Buffer
	err := setup(t.Context(), setupOptions{
		home: recordHome(testRecord(t, "substrate-test")), yes: true,
		local: defaultsRunner(func(ctx context.Context, c installer.Command) ([]byte, error) {
			if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 30*time.Second {
				t.Fatal("local tool probe has no bounded deadline")
			}
			return nil, missing
		}),
		discovery: defaultsRunner(func(context.Context, installer.Command) ([]byte, error) {
			t.Fatal("local failure reached Azure")
			return nil, nil
		}),
	}, strings.NewReader(""), &output, &output, nil)
	if !errors.Is(err, missing) || strings.Contains(output.String(), "billable installation?") {
		t.Fatalf("local failure was hidden or reached consent: %v", err)
	}
}

func TestDoctorNeedsNeitherConfigNorCloud(t *testing.T) {
	t.Chdir(t.TempDir())
	bin := t.TempDir()
	for _, tool := range []string{"az", "git", "go", "kubelogin"} {
		fakeTool(t, bin, tool, `case "$1" in version|--version|bicep) exit 0;; *) exit 19;; esac`)
	}
	fakeTool(t, bin, "kubectl", `printf '%s' '{"clientVersion":{"major":"1","minor":"37"}}'`)
	t.Setenv("PATH", bin)
	var output bytes.Buffer
	if err := run(t.Context(), []string{"doctor"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(".substrate-aks"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config-independent doctor created installation state: %v", err)
	}
}

func TestAzureDiscoveryCallsHaveDeadlines(t *testing.T) {
	for _, probe := range []string{"account", "directory", "regions"} {
		t.Run(probe, func(t *testing.T) {
			called := false
			r := defaultsRunner(func(ctx context.Context, c installer.Command) ([]byte, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > discoveryTimeout {
					t.Fatalf("unbounded Azure probe: %v", c.Args)
				}
				called = true
				switch c.Args[1] {
				case "account":
					return []byte(accountJSON("user")), nil
				case "ad":
					return []byte(`{"id":"` + defaultPublisher + `"}`), nil
				default:
					return []byte(testRegionsJSON), nil
				}
			})
			var err error
			if probe == "regions" {
				_, err = readAzureRegions(t.Context(), defaultSubscription, r)
			} else {
				publisher := defaultPublisher
				if probe == "directory" {
					publisher = ""
				}
				_, err = discoverAzureDefaults(t.Context(), "", publisher, r)
			}
			if err != nil || !called {
				t.Fatalf("probe did not run successfully: %v", err)
			}
		})
	}
}
