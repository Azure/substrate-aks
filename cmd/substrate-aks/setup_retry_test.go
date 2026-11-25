package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestSetupRetriesOnlyFailedStageWithIdenticalFlags(t *testing.T) {
	path := testCLIConfig(t)
	var output bytes.Buffer
	var failedArgs []string
	publications, previews := 0, 0
	var published <-chan struct{}
	// Continue, apply the registry, retry the images, then apply the cluster
	// after its new preview.
	err := setupForTest(t.Context(), setupOptions{
		home: recordHome(path), name: recordName(path), containerEngine: "podman", withDemo: true, verbose: true,
		discovery: subscriptionDisplayRunner(t), backgroundStarted: func(done <-chan struct{}) { published = done },
	}, strings.NewReader("yes\nyes\nyes\nyes\n"), &output, &output,
		func(_ context.Context, args []string, _, _ io.Writer) error {
			// The first image failure is raised before the cluster approval.
			if args[0] == "plan" && !slices.Contains(args, "--registry-only") {
				if previews++; previews == 1 {
					return waitFor(published, "image publishing")
				}
			}
			if args[0] == "images" && !slices.Contains(args, "--check") {
				publications++
				if publications == 1 {
					failedArgs = slices.Clone(args)
					return errors.New("temporary publication failure")
				}
				if !slices.Equal(args, failedArgs) {
					t.Fatalf("retry changed command flags: %v vs %v", args, failedArgs)
				}
			}
			return nil
		})
	if err != nil || publications != 2 || strings.Count(output.String(), "Retry this stage") != 1 {
		t.Fatalf("failed-stage retry did not complete: %d publications, %v\n%s", publications, err, &output)
	}
}

func TestSetupFailureGuidancePreservesOptionsAndCancellationNeverRetries(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			var output bytes.Buffer
			calls := 0
			failure := errors.New("stage failed")
			if cancelled {
				failure = context.Canceled
			}
			err := setupForTest(t.Context(), setupOptions{
				home: recordHome(testCLIConfig(t)), name: "substrate-test", yes: !cancelled, containerEngine: "podman",
				withDemo: true, verbose: true, accessible: true, skipContextHandoff: true,
				discovery: subscriptionDisplayRunner(t),
			}, strings.NewReader("yes\n"), &output, &output,
				func(context.Context, []string, io.Writer, io.Writer) error { calls++; return failure })
			if !errors.Is(err, failure) || calls != 1 || strings.Contains(output.String(), "Retry this stage") {
				t.Fatalf("automation or cancellation retried: calls=%d, error=%v", calls, err)
			}
			for _, want := range []string{"Retry failed stage:", "Full reconciliation", "--container-engine podman", "--with-demo", "--verbose", "--accessible", "--skip-context-handoff"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("recovery guidance lost %q: %v", want, err)
				}
			}

		})
	}
}

// A stage's own deadline, such as the operator-access wait in check, is a
// failure the operator can retry while setup itself is still running.
func TestSetupOffersRetryForStageDeadline(t *testing.T) {
	var output bytes.Buffer
	calls := 0
	// The retried stage is cancelled, which ends setup without another offer.
	err := setupForTest(t.Context(), setupOptions{
		home: recordHome(testCLIConfig(t)), name: "substrate-test", withDemo: true, discovery: subscriptionDisplayRunner(t),
	}, strings.NewReader("yes\nyes\n"), &output, &output,
		func(context.Context, []string, io.Writer, io.Writer) error {
			if calls++; calls == 1 {
				return fmt.Errorf("operator access was denied: %w", context.DeadlineExceeded)
			}
			return context.Canceled
		})
	if !errors.Is(err, context.Canceled) || calls != 2 || strings.Count(output.String(), "Retry this stage") != 1 {
		t.Fatalf("stage deadline was not offered a retry: calls=%d, error=%v\n%s", calls, err, &output)
	}
}

func TestSetupFinalHandoffCancellationRetainsRecoveryGuidance(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var output bytes.Buffer
	err := setupForTest(ctx, setupOptions{
		home: recordHome(testCLIConfig(t)), name: "substrate-test", withDemo: true, discovery: subscriptionDisplayRunner(t),
	}, strings.NewReader("yes\nyes\nyes\n"), &output, &output,
		func(_ context.Context, args []string, _, _ io.Writer) error {
			if args[0] == "client" {
				cancel()
			}
			return nil
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("final handoff cancellation was lost: %v", err)
	}
	for _, want := range []string{"Resources are retained", "Retry failed stage:", " client substrate-test", "--use-context", "Full reconciliation", "Teardown:"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("handoff cancellation lost %q: %v", want, err)
		}
	}
	if strings.Contains(output.String(), "Setup complete") || strings.Contains(output.String(), "export KUBECONFIG=") {
		t.Fatal("cancelled handoff printed success or premature exports")
	}
}
