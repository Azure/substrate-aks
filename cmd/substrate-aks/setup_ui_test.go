package main

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Azure/substrate-aks/internal/installer"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func TestSetupUIStylesAndReadableSummary(t *testing.T) {
	c, err := installer.LoadConfig(testCLIConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, colored := range []bool{false, true} {
		t.Run(fmt.Sprintf("colored=%v", colored), func(t *testing.T) {
			var output bytes.Buffer
			ui := newSetupUI(&output)
			if colored {
				ui.renderer.SetColorProfile(termenv.ANSI)
			}
			ui.welcome(false)
			ui.section("Configure your installation")
			ui.summary(c, azureDefaults{subscriptionName: "Development", publisherName: "Publisher"}, "deploy.json", true, "podman (auto-detected)")
			text := output.String()
			plain := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(text, "")
			compact := strings.Join(strings.Fields(plain), " ")
			for _, want := range []string{
				"Substrate on AKS",
				"Installation summary", "Configuration saved: deploy.json", "No Azure resources have been created.",
				"Development (" + c.Azure.SubscriptionID + ")", "Azure identity", c.Azure.AssetPublisherObjectID,
				"Resource group", "Region", "1 system + 1 microVM", "API access",
				"Snapshot inspection", "No new operator access", "Existing operator grants are not revoked.",
				"WARNING: This creates billable ACR, AKS, and storage resources.",
				"Teardown deletes all installation data.",
			} {
				if !strings.Contains(compact, want) {
					t.Fatalf("welcome or summary lost %q:\n%s", want, plain)
				}
			}
			if strings.Contains(text, "\x1b[") != colored {
				t.Fatalf("incorrect setup color profile: %q", text)
			}
		})
	}
}

func TestSetupWelcomeBanner(t *testing.T) {
	for _, tc := range []struct {
		name       string
		width      int
		decorative bool
		wantArt    bool
	}{
		{"wide", 80, true, true},
		{"minimum width", 64, true, true},
		{"narrow", 63, true, false},
		{"plain", 80, false, false},
	} {
		for _, colored := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/colored=%v", tc.name, colored), func(t *testing.T) {
				var output bytes.Buffer
				ui := newSetupUI(&output)
				ui.width = tc.width
				if colored {
					ui.renderer.SetColorProfile(termenv.ANSI)
				}
				ui.welcome(tc.decorative)
				text := output.String()
				plain := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(text, "")
				if strings.Contains(plain, "s u b s t r a t e") != tc.wantArt {
					t.Fatalf("incorrect decorative/plain welcome:\n%s", plain)
				}
				if !tc.wantArt && !strings.Contains(plain, "Substrate on AKS") {
					t.Fatalf("plain welcome lost its heading:\n%s", plain)
				}
				for _, line := range strings.Split(plain, "\n") {
					if lipgloss.Width(line) > tc.width {
						t.Fatalf("banner overflowed %d columns: %q", tc.width, line)
					}
				}
				if strings.Contains(text, "\x1b[") != colored {
					t.Fatalf("incorrect banner color profile: %q", text)
				}
				if tc.name == "wide" && !colored {
					t.Log("\n" + plain)
				}
			})
		}
	}
}

func TestSetupWelcomeUsesPurpleArtworkAndText(t *testing.T) {
	var output bytes.Buffer
	ui := newSetupUI(&output)
	ui.renderer.SetColorProfile(termenv.TrueColor)
	ui.welcome(true)
	lines := strings.Split(strings.TrimPrefix(output.String(), "\n"), "\n")
	if len(lines) < 7 {
		t.Fatalf("banner is incomplete:\n%s", &output)
	}
	for _, tc := range []struct {
		row  int
		text string
		bold bool
	}{
		{0, " ____________", false},
		{1, "/___________/|", false},
		{1, "s u b s t r a t e", true},
		{3, "/___________/|", false},
		{3, "on AKS", false},
		{5, "/___________/|", false},
		{5, "An AKS packaging layer", false},
		{6, "for Agent Substrate.", false},
	} {
		styled := ui.renderer.NewStyle().Foreground(lipgloss.Color("#b48cf2")).Bold(tc.bold).Render(tc.text)
		prefix, _, _ := strings.Cut(styled, tc.text)
		if !strings.Contains(lines[tc.row], prefix+tc.text) {
			t.Fatalf("banner element %q has the wrong color or alignment: %q", tc.text, lines[tc.row])
		}
	}
}

func TestSetupSummaryFitsNarrowTerminalsWithoutLosingIdentifiers(t *testing.T) {
	c, err := installer.LoadConfig(testCLIConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	c.Azure.SnapshotReaderObjectID = c.Azure.AssetPublisherObjectID
	for _, width := range []int{32, 48, 80} {
		t.Run(fmt.Sprintf("width=%d", width), func(t *testing.T) {
			var output bytes.Buffer
			ui := newSetupUI(&output)
			ui.width = width
			ui.welcome(true)
			const path = "/tmp/configuration-directory-with-a-long-name/deploy.json"
			ui.summary(c, azureDefaults{subscriptionName: "A long development subscription name"}, path, true, "podman (auto-detected)")
			for _, line := range strings.Split(output.String(), "\n") {
				if lipgloss.Width(line) > width {
					t.Fatalf("setup overflowed a %d-column terminal: %q", width, line)
				}
			}
			compact := strings.Join(strings.Fields(output.String()), "")
			for _, want := range []string{c.Azure.SubscriptionID, c.Azure.SnapshotReaderObjectID, path,
				"Read-only,snapshotscontaineronly"} {
				if !strings.Contains(compact, want) {
					t.Fatalf("narrow summary truncated %q:\n%s", want, &output)
				}
			}
		})
	}
}

func TestSetupWelcomeSuppressesStandaloneInitInstructions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deploy.json")
	var result, display bytes.Buffer
	err := setupForTest(t.Context(), setupOptions{
		path: path, subscription: defaultSubscription, publisher: defaultPublisher,
		discovery: subscriptionDisplayRunner(t),
		cidr:      "203.0.113.8/32", name: "substrate-test", location: "southcentralus",
	}, strings.NewReader("\n\nno\nno\n"), &result, &display, nil)
	if err == nil || !strings.Contains(err.Error(), "before provisioning") {
		t.Fatalf("unexpected setup result: %v", err)
	}
	plain := strings.Join(strings.Fields(display.String()), " ")
	for _, unwanted := range []string{"30-second", "30 seconds", "Sign in with az login first."} {
		if strings.Contains(plain, unwanted) {
			t.Fatalf("setup leaked internal discovery details or premature sign-in advice: %s", plain)
		}
	}
	for _, want := range []string{
		"Substrate on AKS", "Configure your installation",
		"Snapshot inspection", "Installation summary", "Before you continue",
		"Continue with this billable installation? [y/N]",
	} {
		if !strings.Contains(plain, want) {
			t.Fatalf("setup did not wire its styled onboarding: missing %q in %s", want, &display)
		}
	}
	if result.Len() != 0 || strings.Contains(display.String(), "Next: substrate-aks setup") ||
		strings.Count(display.String(), "203.0.113.8/32") != 1 {
		t.Fatalf("setup leaked redundant standalone-init output: stdout=%q stderr=%q", &result, &display)
	}
}
