package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/substrate-aks/internal/installer"
)

// testRecord isolates the installation store and returns where the named
// installation's record is (or will be) saved.
func testRecord(t *testing.T, name string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv(installer.HomeEnv, home)
	return installer.RecordPath(home, name)
}

func recordHome(path string) string { return filepath.Dir(filepath.Dir(filepath.Dir(path))) }
func recordName(path string) string { return filepath.Base(filepath.Dir(path)) }

func testCLIConfig(t *testing.T) string {
	t.Helper()
	path := testRecord(t, "substrate-test")
	err := initialize(recordHome(path), "22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333", "", "203.0.113.8/32", "substrate-test", "southcentralus", false, nil, "", "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeTool(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestCLIQuietAndVerboseDiagnostics(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		t.Run(map[bool]string{false: "quiet", true: "verbose"}[verbose], func(t *testing.T) {
			path := testCLIConfig(t)
			bin := t.TempDir()
			fakeTool(t, bin, "az", "printf 'bicep-diagnostic-only\\n'\n")
			fakeTool(t, bin, "git", "exit 0\n")
			fakeTool(t, bin, "go", "printf 'go-diagnostic-only\\n'\n")
			for _, tool := range []string{"bash", "curl", "tar", "zstd", "readlink", "sha256sum", "kubelogin"} {
				fakeTool(t, bin, tool, "exit 0")
			}
			fakeTool(t, bin, "kubectl", `printf '{"clientVersion":{"major":"1","minor":"37"},"extra":"kubectl-diagnostic-only"}'`+"\n")
			t.Setenv("PATH", bin)
			args := []string{"doctor", recordName(path)}
			if verbose {
				args = append(args, "--verbose")
			}
			var out, console bytes.Buffer
			if err := run(t.Context(), args, &out, &console); err != nil {
				t.Fatal(err)
			}
			if out.Len() != 0 {
				t.Fatalf("progress/diagnostics polluted result stdout: %s", out.String())
			}
			if !strings.Contains(console.String(), "Checking Azure Bicep") || !strings.Contains(console.String(), "Local prerequisites complete") {
				t.Fatalf("missing informative progress or completion: %s", console.String())
			}
			for _, marker := range []string{"bicep-diagnostic-only", "go-diagnostic-only", "kubectl-diagnostic-only", `$ "az"`} {
				if strings.Contains(console.String(), marker) != verbose {
					t.Fatalf("incorrect console verbosity for %q: %s", marker, console.String())
				}
			}
			logs, err := filepath.Glob(filepath.Join(filepath.Dir(path), "logs", "*.log"))
			if err != nil || len(logs) != 1 {
				t.Fatalf("missing per-command diagnostic log: %v, %v", logs, err)
			}
			log, err := os.ReadFile(logs[0])
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(log, []byte("kubectl-diagnostic-only")) || !bytes.Contains(log, []byte("[step] Checking Azure Bicep")) {
				t.Fatalf("quiet mode lost diagnostics or stage context: %s", log)
			}
			info, err := os.Stat(logs[0])
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("diagnostic log must remain private: %v, %v", info, err)
			}
		})
	}
}

func TestCLIOwnershipFailureIsActionableAndLogged(t *testing.T) {
	path := testCLIConfig(t)
	c, err := installer.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	fakeTool(t, bin, "az", `
case "$1 $2" in
  "account show") printf '{"id":"22222222-2222-4222-8222-222222222222"}';;
  "group list") printf '[]';;
  "group exists") printf 'true';;
  "group show") printf '{"location":"eastus2","tags":{"purpose":"not-owned"}}';;
  *) printf 'unexpected Azure operation\n' >&2; exit 19;;
esac
`)
	t.Setenv("PATH", bin)
	var out, console bytes.Buffer
	err = run(t.Context(), []string{"plan", recordName(path)}, &out, &console)
	if err == nil {
		t.Fatal("unowned resource group was accepted")
	}
	for _, want := range []string{"Infrastructure preview failed", "Checking resource group ownership", c.Azure.ResourceGroup, "eastus2", "choose an unused azure.resourceGroup", "has not been modified", "Diagnostic log:"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("failure lacks %q: %v", want, err)
		}
	}
	if strings.Contains(console.String(), `$ "az"`) || strings.Contains(console.String(), `"tags"`) ||
		strings.Contains(console.String(), "complete") || out.Len() != 0 {
		t.Fatalf("failure leaked machinery or claimed success: console=%s, stdout=%s", console.String(), out.String())
	}
	_, logPath, _ := strings.Cut(err.Error(), "Diagnostic log: ")
	log, readErr := os.ReadFile(logPath)
	if readErr != nil || !bytes.Contains(log, []byte("not-owned")) || !bytes.Contains(log, []byte("[error]")) {
		t.Fatalf("failure diagnostics not persisted: %s, %v", log, readErr)
	}
}

func TestHeartbeatUsesCurrentStep(t *testing.T) {
	var output bytes.Buffer
	p := newProgress(&output, io.Discard, "Starting", true)
	defer p.stop()
	p.step("Old operation")
	p.step("Waiting for AKS")
	p.mu.Lock()
	output.Reset()
	p.mu.Unlock()
	p.heartbeat()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !strings.Contains(output.String(), "Still working: Waiting for AKS") ||
		strings.Contains(output.String(), "Old operation") {
		t.Fatalf("long-running operation has stale progress: %s", output.String())
	}
}

func TestSpinnerAdvancesAndClearsOnStop(t *testing.T) {
	var output, log bytes.Buffer
	p := newProgress(&output, &log, "Starting", false)
	p.step("Building images")
	for range 12 {
		p.spin()
	}
	p.stop()
	text := output.String()
	for _, frame := range []string{"\r\x1b[2K  ⠋ ", "\r\x1b[2K  ⠙ "} {
		if strings.Count(text, frame) != 2 {
			t.Fatalf("spinner did not advance and wrap frames: %q", text)
		}
	}
	if !strings.HasSuffix(text, "\r\x1b[2K") || strings.Count(text, "Building images") != 1 {
		t.Fatalf("spinner was not cleared or repeated its step message: %q", text)
	}
	if strings.Contains(log.String(), "\x1b") || !strings.Contains(log.String(), "[step] Building images") {
		t.Fatalf("spinner polluted diagnostics or lost stage context: %q", log.String())
	}
}

func TestSpinnerClearsBeforeOtherOutput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(*progress)
		want  string
	}{
		{"step", func(p *progress) { p.step("Next step") }, "STEP  Next step..."},
		{"info", func(p *progress) { p.info("Metadata") }, "INFO  Metadata"},
		{"warning", func(p *progress) { p.warn("Warning") }, "WARN  Warning"},
		{"completion", func(p *progress) { p.complete("Complete") }, "DONE  Complete"},
		{"raw output", func(p *progress) { _, _ = p.Write([]byte("raw output\n")) }, "raw output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			p := newProgress(&output, io.Discard, "Starting", false)
			p.step("Waiting")
			p.spin()
			tc.write(p)
			p.stop()
			if !strings.Contains(output.String(), "\r\x1b[2K"+tc.want) {
				t.Fatalf("output collided with the active spinner: %q", output.String())
			}
		})
	}
}

func TestPausedProgressStaysSilentUntilNextStep(t *testing.T) {
	var output bytes.Buffer
	p := newProgress(&output, io.Discard, "Starting", false)
	defer p.stop()
	p.step("Preparing results")
	p.spin()
	p.pause()
	before := output.String()
	p.spin()
	p.heartbeat()
	if output.String() != before {
		t.Fatalf("progress resumed while results were being printed: %q", output.String())
	}
	p.step("Next operation")
	p.spin()
	p.pause()
	if !strings.Contains(output.String()[len(before):], "\r\x1b[2K  ⠙ ") {
		t.Fatalf("next step did not resume animation: %q", output.String())
	}
}
