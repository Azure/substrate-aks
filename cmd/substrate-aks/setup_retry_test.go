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
	publications := 0
	err := setupForTest(t.Context(), setupOptions{
		path: path, containerEngine: "podman", withDemo: true, verbose: true,
		discovery: subscriptionDisplayRunner(t),
	}, strings.NewReader("yes\nyes\nyes\nyes\n"), &output, &output,
		func(_ context.Context, args []string, _, _ io.Writer) error {
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
				path: testCLIConfig(t), yes: !cancelled, containerEngine: "podman",
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
