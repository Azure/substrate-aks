package main

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/Azure/substrate-aks/internal/installer"
)

func TestPlanTablePreservesScopeAndSeparatesSections(t *testing.T) {
	const subscription = "22222222-2222-4222-8222-222222222222"
	const group = "/resourceGroups/example"
	const registry = group + "/providers/Microsoft.ContainerRegistry/registries/example"
	const role = registry + "/providers/Microsoft.Authorization/roleAssignments/grant"
	const foreign = "/subscriptions/other/resourceGroups/example/providers/Microsoft.ContainerRegistry/registries/foreign"
	changes := []installer.ResourceChange{
		{ChangeType: "Create", ResourceID: "/subscriptions/" + subscription + group},
		{ChangeType: "Create", ResourceID: "/SUBSCRIPTIONS/" + subscription + registry},
		{ChangeType: "Create", ResourceID: "/subscriptions/" + subscription + role},
		{ChangeType: "Delete", ResourceID: foreign},
		{ChangeType: "NoChange", ResourceID: "unchanged"},
	}
	var output bytes.Buffer
	if err := writePlan(&output, subscription, changes); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, id := range []string{group, registry, role, foreign} {
		if !strings.Contains(text, id) {
			t.Fatalf("table lost scope for %q: %s", id, text)
		}
	}
	if strings.Count(text, "/subscriptions/"+subscription) != 1 || strings.Contains(text, "unchanged") || strings.Contains(text, "\x1b") {
		t.Fatalf("IDs were not uniformly relative, unchanged rows leaked, or redirected output was styled: %q", text)
	}
	for _, want := range []string{"\nPlanned resource changes\n", "Scope: /subscriptions/" + subscription,
		"Change", "Resource ID", "+ Create", "- Delete", "\n\nPreview only; nothing deployed.", "--verbose).\n\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing table content or section spacing %q: %s", want, text)
		}
	}
}

func TestPlanTableStylesChangesAndKeepsUnknownsVisible(t *testing.T) {
	renderer := lipgloss.NewRenderer(io.Discard)
	renderer.SetColorProfile(termenv.ANSI)
	var changes []installer.ResourceChange
	for _, changeType := range []string{"Create", "Delete", "Modify", "Deploy", "Ignore", "Unsupported", "FutureChange"} {
		changes = append(changes, installer.ResourceChange{ChangeType: changeType, ResourceID: "/resourceGroups/" + changeType})
	}
	text := renderPlan(renderer, "example", changes, 0)
	for _, pair := range []struct {
		pattern string
		label   string
	}{
		{`\x1b\[[0-9;]*32m\+ Create`, "green create"},
		{`\x1b\[[0-9;]*31m- Delete`, "red delete"},
		{`\x1b\[[0-9;]*33m~ Modify`, "yellow modify"},
		{`\x1b\[[0-9;]*36m> Deploy`, "cyan deploy"},
		{`\x1b\[[0-9;]*33m\? Ignore`, "uncertain ignored resource"},
		{`\x1b\[[0-9;]*33m\? Unsupported`, "unsupported resource"},
		{`\x1b\[[0-9;]*33m\? FutureChange`, "unknown change type"},
	} {
		if !regexp.MustCompile(pair.pattern).MatchString(text) {
			t.Fatalf("missing %s: %q", pair.label, text)
		}
	}
}

func TestPlanTableWrapsWithoutTruncatingIDsAndHandlesNoChanges(t *testing.T) {
	renderer := lipgloss.NewRenderer(io.Discard)
	renderer.SetColorProfile(termenv.Ascii)
	id := "/resourceGroups/example/providers/Microsoft.ContainerRegistry/registries/registry/providers/Microsoft.Authorization/roleAssignments/long-assignment-id"
	text := renderPlan(renderer, "example", []installer.ResourceChange{{ChangeType: "Create", ResourceID: id}}, 80)
	start := strings.Index(text, " Change")
	end := strings.Index(text, "\n\nPreview only;")
	if start < 0 || end <= start {
		t.Fatalf("missing table: %s", text)
	}
	body := text[start:end]
	for _, line := range strings.Split(body, "\n") {
		if len(line) > 80 {
			t.Fatalf("table exceeded terminal width: %q", line)
		}
	}
	compact := strings.NewReplacer(" ", "", "\n", "", "\r", "", "|", "").Replace(body)
	if !strings.Contains(compact, id) {
		t.Fatalf("wrapped table lost resource ID content: %s", body)
	}
	text = renderPlan(renderer, "example", []installer.ResourceChange{{ChangeType: "NoChange", ResourceID: "unchanged"}}, 80)
	if !strings.Contains(text, "\n\nNo resource changes.\n\n") || strings.Contains(text, "unchanged") {
		t.Fatalf("empty preview was unclear or leaked unchanged rows: %s", text)
	}
}

func TestCLIPlanRendersTableOnResultStream(t *testing.T) {
	path := testCLIConfig(t)
	bin := t.TempDir()
	fakeTool(t, bin, "az", `
case "$1 $2" in
  "account show") printf '{"id":"22222222-2222-4222-8222-222222222222"}';;
  "group list") printf '[]';;
  "group exists") printf 'false';;
  "deployment sub") printf '{"status":"Succeeded","changes":[{"changeType":"Create","resourceId":"/subscriptions/22222222-2222-4222-8222-222222222222/resourceGroups/example/providers/Microsoft.ContainerRegistry/registries/example"}]}';;
  *) printf 'unexpected Azure operation\n' >&2; exit 19;;
esac
`)
	t.Setenv("PATH", bin)
	var result, progress bytes.Buffer
	if err := run(t.Context(), []string{"plan", "--registry-only", "--config", path}, &result, &progress); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Planned resource changes", "Resource ID", "+ Create",
		"/resourceGroups/example/providers/Microsoft.ContainerRegistry/registries/example",
		"\n\nPreview only; nothing deployed."} {
		if !strings.Contains(result.String(), want) {
			t.Fatalf("CLI did not render the preview table on stdout: missing %q in %s", want, result.String())
		}
	}
	if !strings.Contains(progress.String(), "Infrastructure preview complete") ||
		strings.Contains(result.String(), "STEP") || strings.Contains(progress.String(), "+ Create") {
		t.Fatalf("CLI mixed progress and plan results: stdout=%s stderr=%s", result.String(), progress.String())
	}
}
