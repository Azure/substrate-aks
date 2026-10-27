package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/substrate-aks/internal/installer"
)

const testRegionsJSON = `["eastus2","southcentralus"]`

func isRegionList(c installer.Command) bool {
	return len(c.Args) > 1 && c.Args[0] == "az" && c.Args[1] == "rest"
}

func TestReadAzureRegionsTargetsSelectedSubscription(t *testing.T) {
	runner := defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
		want := []string{"az", "rest", "--method", "get",
			"--url", "https://management.azure.com/subscriptions/" + defaultSubscription + "/locations?api-version=2022-12-01",
			"--query", "value[?metadata.regionType=='Physical'].name", "--only-show-errors", "--output", "json"}
		if !reflect.DeepEqual(c.Args, want) {
			t.Fatalf("catalog lookup did not target the configured subscription's physical regions: %v", c.Args)
		}
		return []byte(testRegionsJSON), nil
	})
	regions, err := readAzureRegions(t.Context(), defaultSubscription, runner)
	if err != nil || len(regions) != 2 {
		t.Fatalf("cannot read region catalog: %v, %v", regions, err)
	}
}

func TestAzureRegionValidation(t *testing.T) {
	regions := []string{"eastus2", "southcentralus"}
	for _, tc := range []struct{ input, want, problem string }{
		{"eastus2", "eastus2", ""},
		{" SouthCentralUS ", "southcentralus", ""},
		{"southcen", "", "possible matches: southcentralus"},
		{"nowhere", "", "unknown Azure region"},
		{"", "", "unknown Azure region"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := validateAzureRegion(tc.input, regions)
			if got != tc.want || (tc.problem == "" && err != nil) ||
				(tc.problem != "" && (err == nil || !strings.Contains(err.Error(), tc.problem))) {
				t.Fatalf("region validation = %q, %v", got, err)
			}
		})
	}
}

func TestAzureRegionCatalogFailsClosed(t *testing.T) {
	for _, response := range []string{"null", "[]", "{}", `[""]`, `["East US"]`, "not json"} {
		t.Run(response, func(t *testing.T) {
			_, err := readAzureRegions(t.Context(), defaultSubscription,
				defaultsRunner(func(context.Context, installer.Command) ([]byte, error) { return []byte(response), nil }))
			if err == nil {
				t.Fatal("accepted unusable Azure region catalog")
			}
		})
	}
	want := errors.New("catalog access denied")
	_, err := readAzureRegions(t.Context(), defaultSubscription,
		defaultsRunner(func(context.Context, installer.Command) ([]byte, error) { return nil, want }))
	if !errors.Is(err, want) {
		t.Fatalf("catalog failure was hidden: %v", err)
	}
}

func TestSetupRejectsInvalidRegionBeforeSaveOrBilling(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(map[bool]string{false: "new automation", true: "saved configuration"}[saved], func(t *testing.T) {
			path := testRecord(t, "substrate-test")
			var before []byte
			if saved {
				var out bytes.Buffer
				if err := initialize(recordHome(path), defaultSubscription, defaultPublisher, "", "203.0.113.8/32",
					"substrate-test", "southcen", false, nil, "", "", installer.DatabaseConfig{}, &out); err != nil {
					t.Fatal(err)
				}
				var err error
				before, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			err := setupForTest(t.Context(), setupOptions{
				home: recordHome(path), name: recordName(path), subscription: defaultSubscription, publisher: defaultPublisher, yes: !saved,
				cidr: "203.0.113.8/32", location: "southcen",
				discovery: subscriptionDisplayRunner(t),
			}, strings.NewReader("yes\n"), &output, &output,
				func(context.Context, []string, io.Writer, io.Writer) error {
					t.Fatal("invalid region reached deployment stages")
					return nil
				})
			if err == nil || !strings.Contains(err.Error(), "southcentralus") ||
				strings.Contains(output.String(), "Continue with this billable installation") {
				t.Fatalf("invalid region reached billing confirmation: %v\n%s", err, &output)
			}
			after, readErr := os.ReadFile(path)
			if saved {
				if readErr != nil || !bytes.Equal(before, after) || !strings.Contains(err.Error(), "edit azure.location") {
					t.Fatalf("invalid saved region was silently rewritten: %v, %v", err, readErr)
				}
			} else if !errors.Is(readErr, os.ErrNotExist) {
				t.Fatalf("invalid region was persisted: %v", readErr)
			}
		})
	}
}

func TestInitRejectsUnknownRegionBeforeSaving(t *testing.T) {
	bin := t.TempDir()
	fakeTool(t, bin, "az", `test "$1" = rest || exit 19; printf '%s' '`+testRegionsJSON+`'`)
	t.Setenv("PATH", bin)
	path := testRecord(t, "substrate-test")
	var out bytes.Buffer
	err := run(t.Context(), []string{"init", recordName(path), "--subscription", defaultSubscription,
		"--identity-object-id", defaultPublisher, "--operator-object-id", defaultPublisher,
		"--location", "southcen", "--api-cidr", "203.0.113.8/32"}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "southcentralus") {
		t.Fatalf("init accepted an unknown region: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid region was persisted: %v", err)
	}
}

func TestSetupCatalogFailureCreatesNoConfiguration(t *testing.T) {
	path := testRecord(t, "substrate-test")
	denied := errors.New("region catalog access denied")
	var output bytes.Buffer
	err := setupForTest(t.Context(), setupOptions{
		home: recordHome(path), name: recordName(path), subscription: defaultSubscription, publisher: defaultPublisher,
		yes: true, cidr: "203.0.113.8/32", location: "eastus2",
		discovery: defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
			if !isRegionList(c) {
				t.Fatalf("unexpected Azure command: %v", c.Args)
			}
			return nil, denied
		}),
	}, strings.NewReader(""), &output, &output, func(context.Context, []string, io.Writer, io.Writer) error {
		t.Fatal("catalog failure reached deployment stages")
		return nil
	})
	if !errors.Is(err, denied) {
		t.Fatalf("catalog failure was hidden: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unvalidated region was persisted: %v", err)
	}
}
