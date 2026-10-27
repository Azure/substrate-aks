package main

import (
	"bytes"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/muesli/termenv"
)

func TestProgressLabelsAndColorStyles(t *testing.T) {
	for _, colored := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "colored"}[colored], func(t *testing.T) {
			var output, diagnostics bytes.Buffer
			p := newProgress(&output, &diagnostics, "Starting", true)
			defer p.stop()
			if colored {
				p.console.logger.SetColorProfile(termenv.ANSI)
			}
			p.heading("Infrastructure provisioning")
			p.info("Target: example / southcentralus")
			p.step("Checking ownership")
			p.warn("Billable resources will be created")
			p.complete("Infrastructure provisioning complete")
			p.mu.Lock()
			p.console.logger.Error("Permission denied")
			rendered := output.String()
			p.mu.Unlock()

			plain := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(rendered, "")
			for _, want := range []string{
				"Infrastructure provisioning", "INFO  Target: example / southcentralus",
				"STEP  Checking ownership...", "WARN  Billable resources will be created",
				"DONE  Infrastructure provisioning complete", "ERROR Permission denied",
			} {
				if !strings.Contains(plain, want) {
					t.Fatalf("missing readable category %q: %q", want, rendered)
				}
			}
			if strings.Contains(rendered, "\x1b[") != colored {
				t.Fatalf("incorrect console styling: %q", rendered)
			}
			if colored {
				for _, label := range []string{"STEP", "WARN", "DONE", "ERROR"} {
					if !regexp.MustCompile(`\x1b\[[0-9;]*m` + label).MatchString(rendered) {
						t.Fatalf("%s label has no style: %q", label, rendered)
					}
				}
			}
			if strings.Contains(diagnostics.String(), "\x1b") || !strings.Contains(diagnostics.String(), "[step] Checking ownership") {
				t.Fatalf("console styling polluted diagnostic logs: %q", diagnostics.String())
			}
		})
	}
}

func TestRedirectedConsoleDisablesStylesEvenWhenColorIsForced(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CLICOLOR_FORCE", "1")
	for _, noColor := range []string{"", "1"} {
		t.Run("NO_COLOR="+noColor, func(t *testing.T) {
			t.Setenv("NO_COLOR", noColor)
			file, err := os.CreateTemp(t.TempDir(), "console-*.log")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { file.Close() })
			c := newConsole(file)
			c.heading.Print("Heading")
			c.logger.Info("Progress")
			c.logger.Warn("Warning")
			c.success.Info("Complete")
			c.logger.Error("Failure")
			ui := newSetupUI(file)
			ui.welcome(false)
			ui.warning("Billable resources")
			prompt := newSetupPrompt(strings.NewReader("\n"), ui, true)
			if _, err := prompt.text(t.Context(), "Resource group", "substrate-dev"); err != nil {
				t.Fatal(err)
			}
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(file)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte{0x1b}) || !bytes.Contains(data, []byte("DONE  Complete")) {
				t.Fatalf("redirected output was styled or lost its status labels: %q", data)
			}
		})
	}
}
