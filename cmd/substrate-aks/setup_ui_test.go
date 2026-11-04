package main

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Azure/substrate-aks/internal/installer"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
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
				"Resource group", "Region", "1 system + 1 microVM", "API server access",
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

func TestSetupCompletionBanner(t *testing.T) {
	const cluster = "substrate-cluster-with-a-long-name"
	for _, width := range []int{24, 32, 48, 80} {
		for _, colored := range []bool{false, true} {
			for _, demo := range []bool{false, true} {
				t.Run(fmt.Sprintf("width=%d/color=%v/demo=%v", width, colored, demo), func(t *testing.T) {
					var output bytes.Buffer
					ui := newSetupUI(&output)
					ui.width = width
					if colored {
						ui.renderer.SetColorProfile(termenv.ANSI)
					}
					ui.complete(cluster, demo)
					text := output.String()
					plain := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(text, "")
					compact := strings.Join(strings.Fields(strings.NewReplacer("|", "", "│", "").Replace(plain)), "")
					for _, want := range []string{"substrateonaks", "donesetupcomplete.", cluster} {
						if !strings.Contains(strings.ToLower(compact), want) {
							t.Fatalf("completion banner lost %q:\n%s", want, plain)
						}
					}
					if strings.Contains(compact, "CountermicroVMdemoinstalled.") != demo {
						t.Fatalf("completion banner misstated demo deployment:\n%s", plain)
					}
					for _, line := range strings.Split(plain, "\n") {
						if lipgloss.Width(line) > width {
							t.Fatalf("completion banner overflowed %d columns: %q", width, line)
						}
					}
					if strings.Contains(text, "\x1b[") != colored {
						t.Fatalf("incorrect completion color profile: %q", text)
					}
					if width == 80 && !colored && demo {
						t.Log("\n" + plain)
					}
				})
			}
		}
	}
}

func TestDemoCommandsAreStyledWithoutChangingShellSyntax(t *testing.T) {
	const create = "kubectl ate create actor my-counter \\\n  -a ate-demo-counter-microvm --template counter-microvm"
	const post = "curl --fail-with-body -X POST \\\n  -H 'ate-target-actor: ate-demo-counter-microvm/my-counter' \\\n  http://localhost:8000/"
	for _, colored := range []bool{false, true} {
		t.Run(fmt.Sprint(colored), func(t *testing.T) {
			var output bytes.Buffer
			ui := newSetupUI(&output)
			ui.width = 24
			if colored {
				ui.renderer.SetColorProfile(termenv.ANSI)
			}
			ui.demo()
			text := output.String()
			plain := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(text, "")
			for _, want := range []string{
				"# Try the counter demo:", "# 1. Create a counter actor", "# 5. Activate again",
				create, post, "kubectl -n ate-system port-forward svc/atenet-router 8000:80",
				"kubectl ate suspend actor my-counter \\\n  -a ate-demo-counter-microvm --mode=sync",
			} {
				if !strings.Contains(plain, want) {
					t.Fatalf("demo instructions lost or wrapped %q:\n%s", want, plain)
				}
			}
			if strings.Count(plain, post) != 2 {
				t.Fatal("demo omitted the before/after state-preservation requests")
			}
			firstLine, _, _ := strings.Cut(create, "\n")
			styled := ui.renderer.NewStyle().Foreground(lipgloss.Color(setupAccent)).Render(firstLine)
			if !strings.Contains(text, styled) || strings.Contains(text, "\x1b[") != colored {
				t.Fatalf("demo commands have the wrong color profile: %q", text)
			}
		})
	}
}

func TestDemoCommandsCanBeCopiedIntoAShell(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, tool := range []string{"kubectl", "curl"} {
		fakeTool(t, bin, tool, "printf '"+tool+"'; printf '|%s' \"$@\"; printf '\\n'")
	}
	t.Setenv("PATH", bin)
	var instructions bytes.Buffer
	newSetupUI(&instructions).demo()
	command := exec.CommandContext(t.Context(), shell, "-eu")
	command.Stdin = strings.NewReader(instructions.String())
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("copying the demo instructions into a shell failed: %v\n%s", err, output)
	}
	post := "curl|--fail-with-body|-X|POST|-H|ate-target-actor: ate-demo-counter-microvm/my-counter|http://localhost:8000/"
	want := strings.Join([]string{
		"kubectl|ate|create|actor|my-counter|-a|ate-demo-counter-microvm|--template|counter-microvm",
		"kubectl|-n|ate-system|port-forward|svc/atenet-router|8000:80",
		post,
		"kubectl|ate|suspend|actor|my-counter|-a|ate-demo-counter-microvm|--mode=sync",
		post,
	}, "\n") + "\n"
	if string(output) != want {
		t.Fatalf("demo rendering changed shell arguments:\ngot: %s\nwant: %s", output, want)
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
		{"minimum width", 59, true, true},
		{"narrow", 58, true, false},
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
				if strings.Contains(plain, "╭─╴ ╷ ╷ ┌─╮") != tc.wantArt {
					t.Fatalf("incorrect decorative/plain welcome:\n%s", plain)
				}
				if !tc.wantArt && !strings.Contains(plain, "Agent Substrate on AKS") {
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

func TestSetupWelcomeUsesBrandArtworkAndText(t *testing.T) {
	for _, tc := range []struct {
		profile termenv.Profile
		mark    bool
	}{{termenv.TrueColor, true}, {termenv.ANSI256, true}, {termenv.ANSI, false}} {
		var output bytes.Buffer
		ui := newSetupUI(&output)
		ui.renderer.SetColorProfile(tc.profile)
		ui.welcome(true)
		lines := strings.Split(strings.TrimPrefix(output.String(), "\n"), "\n")
		if len(lines) < 6 {
			t.Fatalf("banner is incomplete:\n%s", &output)
		}
		plain := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(output.String(), "")
		// The block sits beside the wordmark rows, above the pixel AKS.
		if strings.ContainsAny(ansi.Strip(strings.Join(lines[:3], "")), "▀▄█") != tc.mark {
			t.Fatalf("profile %v drew the isometric mark=%v:\n%s", tc.profile, !tc.mark, plain)
		}
		// Each wordmark glyph carries its own gradient step, starting from the lightest Substrate green.
		first := ui.renderer.NewStyle().Foreground(lipgloss.Color("#b9f0bc")).Render("╭─╮")
		if !strings.Contains(lines[0], first) {
			t.Fatalf("profile %v wordmark lost its gradient: %q", tc.profile, lines[0])
		}
		for _, want := range []string{"╭─╮ ╭─╴ ┌─╴ ┌─╮ ╶┬╴   ╭─╴ ╷ ╷ ┌─╮", "█▀▀█ █▀▀▄ ▄▄█▀", "An AKS packaging layer for Agent Substrate."} {
			if !strings.Contains(plain, want) {
				t.Fatalf("profile %v banner lost %q:\n%s", tc.profile, want, plain)
			}
		}
		// "on" sits one blank row below the wordmark and one above AKS, centered
		// on the wordmark.
		rows := strings.Split(plain, "\n")
		find := func(text string) (int, int) {
			for i, row := range rows {
				if at := strings.Index(row, text); at >= 0 {
					return i, utf8.RuneCountInString(row[:at])
				}
			}
			return -1, -1
		}
		wordRow, wordCol := find("╭─╮ ╭─╴")
		onRow, onCol := find(" on ")
		aksRow, _ := find("▄▀▀▄ █ ▄▀")
		if wordRow < 0 || onRow != wordRow+4 || aksRow != onRow+2 || onCol+1 != wordCol+28 {
			t.Fatalf("profile %v did not center \"on\" between the wordmark and AKS:\n%s", tc.profile, plain)
		}
	}
}

func TestSetupMarkAnimatesAgentsWithoutMovingTheBlock(t *testing.T) {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.TrueColor)
	first := setupMark(r, 0)
	frames := map[string]bool{}
	for frame := range 60 {
		mark := setupMark(r, frame)
		if lipgloss.Width(mark) != lipgloss.Width(first) || lipgloss.Height(mark) != lipgloss.Height(first) {
			t.Fatalf("frame %d resized the mark:\n%s", frame, ansi.Strip(mark))
		}
		frames[mark] = true
	}
	if len(frames) < 10 {
		t.Fatalf("agents barely animate: %d distinct frames in 60", len(frames))
	}
}

func TestSetupAKSKeepsSolidPixelsWithoutColor(t *testing.T) {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.Ascii)
	if got := setupAKS(r); got != "▄▀▀▄ █ ▄▀ ▄█▀▀\n█▀▀█ █▀▀▄ ▄▄█▀" {
		t.Fatalf("monochrome AKS lost pixels:\n%s", got)
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
				"snapshotreader"} {
				if !strings.Contains(compact, want) {
					t.Fatalf("narrow summary truncated %q:\n%s", want, &output)
				}
			}
		})
	}
}

func TestSetupWelcomeSuppressesStandaloneInitInstructions(t *testing.T) {
	path := testRecord(t, "substrate-test")
	var result, display bytes.Buffer
	err := setupForTest(t.Context(), setupOptions{
		home: recordHome(path), name: recordName(path), subscription: defaultSubscription, publisher: defaultPublisher,
		discovery: subscriptionDisplayRunner(t),
		cidr:      "203.0.113.8/32", location: "southcentralus",
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
