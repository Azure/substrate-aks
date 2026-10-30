//go:build linux && tour

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/substrate-aks/internal/installer"
)

func slowRunner(r installer.Runner, d time.Duration) installer.Runner {
	return defaultsRunner(func(ctx context.Context, c installer.Command) ([]byte, error) {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return r.Run(ctx, c)
	})
}

func TestTour(t *testing.T) {
	mode := os.Getenv("TOUR")
	delay, _ := time.ParseDuration(os.Getenv("TOUR_DELAY"))
	if delay == 0 {
		delay = 1500 * time.Millisecond
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	o := setupOptions{withDemo: os.Getenv("TOUR_DEMO") != ""}
	if mode == "fresh" {
		o.path = filepath.Join(t.TempDir(), "deploy.json")
		o.subscription, o.publisher, o.operatorID = defaultSubscription, defaultPublisher, defaultPublisher
		o.cidr, o.location = "203.0.113.8/32", "eastus2"
	} else {
		o.path = testCLIConfig(t)
	}
	o.discovery = slowRunner(subscriptionDisplayRunner(t), delay/4)
	o.azure = slowRunner(successfulCloudRunner(o.path), delay/6)
	fails := os.Getenv("TOUR_FAIL")
	attempts := 0
	err = setupForTest(t.Context(), o, tty, os.Stdout, tty, func(ctx context.Context, args []string, _, _ io.Writer) error {
		s := setupSessionFrom(ctx)
		steps := map[string][]string{
			"plan":      {"Validating Bicep template", "Running subscription what-if"},
			"provision": {"Submitting deployment substrate-tui-1030", "Waiting for Microsoft.ContainerService/managedClusters (3m12s)", "Waiting for role assignments to propagate"},
			"images":    {"Building ate-apiserver with ko", "Pushing atelet:sha-4f2a1c (412 MB)", "Assembling microVM rootfs.img"},
			"install":   {"Applying ate-system CRDs", "Waiting for deployment/ate-apiserver rollout"},
			"demo":      {"Deploying ate-demo-counter-microvm", "Warming counter template"},
			"client":    {"Building kubectl-ate", "Writing private kubeconfig"},
		}
		logFile, logErr := os.CreateTemp(t.TempDir(), args[0]+"-*.log")
		if logErr != nil {
			return logErr
		}
		defer logFile.Close()
		if s != nil {
			s.send(setupLogMsg(logFile.Name()))
		}
		fmt.Fprintf(logFile, "[command] %s\n[config] %s\n[target] substrate-test / southcentralus\n", args[0], o.path)
		for _, step := range steps[args[0]] {
			if s != nil {
				s.send(setupStepMsg(step))
			}
			fmt.Fprintf(logFile, "\n[step] %s\n$ \"az\" \"resource\" \"list\" \"--resource-group\" \"substrate-test\" \"--output\" \"json\"\n[\n  {\n    \"name\": \"substrate-test\",\n    \"provisioningState\": \"Succeeded\"\n  }\n]\n", step)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if args[0] == "plan" {
			changes := []installer.ResourceChange{
				{ChangeType: "Create", ResourceID: "/subscriptions/test/resourceGroups/substrate-test"},
				{ChangeType: "Create", ResourceID: "/subscriptions/test/resourceGroups/substrate-test/providers/Microsoft.ContainerRegistry/registries/substratetestacr"},
			}
			if !strings.Contains(strings.Join(args, " "), "--registry-only") {
				changes = append(changes,
					installer.ResourceChange{ChangeType: "Create", ResourceID: "/subscriptions/test/resourceGroups/substrate-test/providers/Microsoft.ContainerService/managedClusters/substrate-test"},
					installer.ResourceChange{ChangeType: "Create", ResourceID: "/subscriptions/test/resourceGroups/substrate-test/providers/Microsoft.Storage/storageAccounts/substratetestsa"},
					installer.ResourceChange{ChangeType: "Create", ResourceID: "/subscriptions/test/resourceGroups/substrate-test/providers/Microsoft.Storage/storageAccounts/substratetestsa/blobServices/default"},
					installer.ResourceChange{ChangeType: "Create", ResourceID: "/subscriptions/test/resourceGroups/substrate-test/providers/Microsoft.Storage/storageAccounts/substratetestsa/blobServices/default/containers/snapshots"},
					installer.ResourceChange{ChangeType: "Create", ResourceID: "/subscriptions/test/resourceGroups/substrate-test/providers/Microsoft.Storage/storageAccounts/substratetestsa/blobServices/default/containers/assets"},
					installer.ResourceChange{ChangeType: "Create", ResourceID: "/subscriptions/test/resourceGroups/substrate-test/providers/Microsoft.ManagedIdentity/userAssignedIdentities/atelet/federatedIdentityCredentials/atelet"},
					installer.ResourceChange{ChangeType: "Create", ResourceID: "/subscriptions/test/resourceGroups/substrate-test/providers/Microsoft.ManagedIdentity/userAssignedIdentities/atelet"},
					installer.ResourceChange{ChangeType: "Create", ResourceID: "/subscriptions/test/resourceGroups/substrate-test/providers/Microsoft.Authorization/roleAssignments/a1"},
					installer.ResourceChange{ChangeType: "Create", ResourceID: "/subscriptions/test/resourceGroups/substrate-test/providers/Microsoft.Authorization/roleAssignments/a2"},
					installer.ResourceChange{ChangeType: "NoChange", ResourceID: "/subscriptions/test/resourceGroups/substrate-test/providers/Microsoft.ContainerRegistry/registries/substratetestacr"},
				)
				if os.Getenv("TOUR_DELETE") != "" {
					changes = append(changes, installer.ResourceChange{ChangeType: "Delete", ResourceID: "/subscriptions/test/resourceGroups/substrate-test/providers/Microsoft.Storage/storageAccounts/oldaccount"})
				}
			}
			return s.preview("test-subscription", changes)
		}
		if args[0] == fails && !strings.Contains(strings.Join(args, " "), "--check") {
			attempts++
			if attempts == 1 {
				return errors.New("pushing atelet:sha-4f2a1c: denied: client with IP 203.0.113.8 is not allowed access")
			}
		}
		return nil
	})
	fmt.Fprintf(os.Stderr, "\nTOUR RESULT: %v\n", err)
	time.Sleep(30 * time.Second)
}
