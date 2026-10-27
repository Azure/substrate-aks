package installer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRunnerPreservesFailureAndLogsBothStreams(t *testing.T) {
	var output bytes.Buffer
	r := ExecRunner{Log: &output}
	data, err := r.Run(t.Context(), Command{
		Args: []string{"sh", "-c", "printf stdout; printf stderr >&2; exit 7"},
	})
	if err == nil {
		t.Fatal("command failure became success")
	}
	_, body, _ := strings.Cut(output.String(), "\n")
	if string(data) != "stdout" || !strings.Contains(body, "stderr") || !strings.Contains(body, "stdout") {
		t.Fatalf("lost command output: stdout=%q, log=%q", data, output.String())
	}
}

func TestRunnerSeparatesDiagnosticsAndResults(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verbose bool
		results bool
	}{
		{name: "quiet"},
		{name: "verbose", verbose: true},
		{name: "results", results: true},
		{name: "verbose results", verbose: true, results: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log, verbose, stdout, stderr bytes.Buffer
			r := ExecRunner{Log: &log, Stdout: &stdout, Stderr: &stderr}
			if tc.verbose {
				r.Verbose = &verbose
			}
			data, err := r.Run(t.Context(), Command{
				Args:       []string{"sh", "-c", "printf result-marker; printf diagnostic-marker >&2"},
				ShowOutput: tc.results,
			})
			if err != nil || string(data) != "result-marker" {
				t.Fatalf("lost captured stdout: %q, %v", data, err)
			}
			_, logBody, _ := strings.Cut(log.String(), "\n")
			for _, marker := range []string{"result-marker", "diagnostic-marker"} {
				if !strings.Contains(logBody, marker) {
					t.Fatalf("log lost %q: %s", marker, log.String())
				}
			}
			if !strings.Contains(log.String(), `$ "sh"`) {
				t.Fatal("log lost command echo")
			}
			if tc.results {
				if stdout.String() != "result-marker" || stderr.String() != "diagnostic-marker" {
					t.Fatalf("incorrect result streams: %q, %q", stdout.String(), stderr.String())
				}
			} else if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatal("internal command output leaked to result streams")
			}
			if tc.verbose != strings.Contains(verbose.String(), `$ "sh"`) {
				t.Fatalf("incorrect verbose command echo: %s", verbose.String())
			}
			// Command echoes include the markers, so inspect only the body.
			_, body, _ := strings.Cut(verbose.String(), "\n")
			wantBody := tc.verbose && !tc.results
			if strings.Contains(body, "result-marker") != wantBody || strings.Contains(body, "diagnostic-marker") != wantBody {
				t.Fatalf("verbose output duplicated results or lost diagnostics: %s", verbose.String())
			}
		})
	}
}

func TestRunnerFailureExcerptIsBoundedAndCancellationPreserved(t *testing.T) {
	var log bytes.Buffer
	r := ExecRunner{Log: &log}
	_, err := r.Run(t.Context(), Command{Args: []string{"sh", "-c", "printf 'first\\nsecond\\nthird\\nfourth\\n' >&2; exit 7"}})
	_, body, _ := strings.Cut(log.String(), "\n")
	if err == nil || strings.Contains(err.Error(), "first") || !strings.Contains(err.Error(), "fourth") ||
		!strings.Contains(body, "first") {
		t.Fatalf("failure excerpt or full diagnostics lost: %v, log %q", err, log.String())
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = r.Run(ctx, Command{Args: []string{"sh", "-c", "exit 0"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestRunnerSensitiveOutputIsCapturedButNeverDisplayed(t *testing.T) {
	for _, exit := range []string{"0", "7"} {
		var log, verbose, stdout, stderr bytes.Buffer
		r := ExecRunner{Log: &log, Verbose: &verbose, Stdout: &stdout, Stderr: &stderr}
		data, err := r.Run(t.Context(), Command{
			Args: []string{"sh", "-c", `printf '%s' "$SYNTHETIC_TOKEN"; printf '%s' "$SYNTHETIC_TOKEN" >&2; exit ` + exit},
			Env:  []string{"SYNTHETIC_TOKEN=private-token-marker"}, SensitiveOutput: true, ShowOutput: true,
		})
		if string(data) != "private-token-marker" || (err == nil) != (exit == "0") {
			t.Fatalf("sensitive command lost captured output or failure: %q, %v", data, err)
		}
		for _, text := range []string{log.String(), verbose.String(), stdout.String(), stderr.String(), fmt.Sprint(err)} {
			if strings.Contains(text, "private-token-marker") {
				t.Fatal("sensitive output leaked into diagnostics or results")
			}
		}
	}
}
