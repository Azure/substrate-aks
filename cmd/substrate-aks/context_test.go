package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/substrate-aks/internal/installer"
)

func writeTestClientKubeconfig(c installer.Config) error {
	config := map[string]any{
		"apiVersion": "v1", "kind": "Config", "current-context": c.AKS.ClusterName,
		"contexts": []any{map[string]any{"name": c.AKS.ClusterName, "context": map[string]any{"cluster": "aks", "user": "operator"}}},
		"clusters": []any{map[string]any{"name": "aks", "cluster": map[string]any{"server": "https://aks.example.invalid"}}},
		"users": []any{map[string]any{"name": "operator", "user": map[string]any{
			"exec": map[string]any{
				"apiVersion": "client.authentication.k8s.io/v1beta1",
				"command":    "kubelogin",
				"args":       []string{"get-token", "--login", "azurecli"},
			},
		}}},
	}

	data, err := json.Marshal(config)
	if err != nil {
		return err
	}
	dir := filepath.Join(c.Installation.StateDir, c.Installation.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "kubeconfig"), data, 0600)
}

func TestSetupYesReportsHandoffFailuresWithoutChangingCredentials(t *testing.T) {
	for _, tc := range []struct{ name, config string }{
		{"invalid destination", "not a kubeconfig"},
		{"conflicting cluster", "apiVersion: v1\nkind: Config\ncurrent-context: previous\nclusters:\n- name: aks\n  cluster:\n    server: https://different.example.invalid\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := testCLIConfig(t)
			c, err := installer.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "config")
			before := []byte(tc.config)
			if err := os.WriteFile(destination, before, 0600); err != nil {
				t.Fatal(err)
			}
			privatePath := filepath.Join(c.Installation.StateDir, c.Installation.ID, "kubeconfig")
			var privateBefore []byte
			var output bytes.Buffer
			err = setupForTest(t.Context(), setupOptions{
				path: path, yes: true, withDemo: true,
				contextFile: destination, discovery: subscriptionDisplayRunner(t),
			}, strings.NewReader(""), &output, &output,
				func(_ context.Context, args []string, _, _ io.Writer) error {
					if args[0] == "client" {
						if err := writeTestClientKubeconfig(c); err != nil {
							return err
						}
						var err error
						privateBefore, err = os.ReadFile(privatePath)
						return err
					}
					return nil
				})
			if err == nil || !strings.Contains(err.Error(), "Substrate is installed, but context handoff") {
				t.Fatalf("unattended handoff failure was not surfaced: %v\n%s", err, &output)
			}
			if strings.Contains(output.String(), "Setup complete.") || strings.Contains(output.String(), "# Try the counter demo:") {
				t.Fatalf("failed handoff printed successful completion:\n%s", &output)
			}
			after, err := os.ReadFile(destination)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("failed handoff changed the destination: %v", err)
			}
			privateAfter, err := os.ReadFile(privatePath)
			if err != nil || len(privateBefore) == 0 || !bytes.Equal(privateBefore, privateAfter) {
				t.Fatalf("failed handoff changed private credentials: %v", err)
			}
		})
	}
}

func TestYesContextHandoffUsesDefaultDestinationWithoutPrompting(t *testing.T) {
	c, err := installer.LoadConfig(testCLIConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTestClientKubeconfig(c); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"KUBECONFIG", "home", "multiple files"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			destination := filepath.Join(home, ".kube", "config")
			t.Setenv("HOME", home)
			t.Setenv("KUBECONFIG", "")
			if mode != "home" {
				destination = filepath.Join(home, "selected-config")
				value := destination
				if mode == "multiple files" {
					value += string(os.PathListSeparator) + filepath.Join(home, "other-config")
				}
				t.Setenv("KUBECONFIG", value)
			}
			var output bytes.Buffer
			ui := newSetupUI(&output)
			selected, err := offerContextSwitch(t.Context(), setupOptions{yes: true}, c,
				newSetupPrompt(strings.NewReader(""), ui, true), ui, &output)
			if mode == "multiple files" {
				if err == nil || selected || !strings.Contains(err.Error(), "--context-file") {
					t.Fatalf("ambiguous destination was not reported: selected=%v err=%v", selected, err)
				}
				if _, err := os.Stat(destination); !os.IsNotExist(err) {
					t.Fatalf("ambiguous handoff created a destination: %v", err)
				}
				return
			}
			if err != nil || !selected || strings.Contains(output.String(), "Import and select this kube-context?") {
				t.Fatalf("--yes did not select the default destination without prompting: selected=%v err=%v\n%s", selected, err, &output)
			}
			selection, err := installer.PlanContextSwitch(destination)
			if err != nil || selection.Previous != c.AKS.ClusterName ||
				!strings.Contains(output.String(), "export KUBECONFIG="+shellQuote(destination)) {
				t.Fatalf("handoff selected the wrong destination or context: %+v, %v\n%s", selection, err, &output)
			}
		})
	}
}

func TestSetupContextHandoffAcceptsYesOrFinalConsent(t *testing.T) {
	for _, mode := range []string{"accept", "decline", "EOF", "automation"} {
		t.Run(mode, func(t *testing.T) {
			path := testCLIConfig(t)
			destination := filepath.Join(t.TempDir(), "config")
			before := []byte("apiVersion: v1\nkind: Config\ncurrent-context: previous\n")
			if err := os.WriteFile(destination, before, 0600); err != nil {
				t.Fatal(err)
			}
			answers := "yes\nyes\nyes\n"
			switch mode {
			case "accept":
				answers += "yes\n"
			case "decline":
				answers += "no\n"
			case "automation":
				answers = ""
			}
			var output bytes.Buffer
			last := ""
			err := setupForTest(t.Context(), setupOptions{
				path: path, yes: mode == "automation", withDemo: true,
				contextFile: destination, discovery: subscriptionDisplayRunner(t),
			}, strings.NewReader(answers), &output, &output,
				func(_ context.Context, args []string, out, _ io.Writer) error {
					last = args[0]
					current, err := os.ReadFile(destination)
					if err != nil || !bytes.Equal(current, before) {
						t.Fatal("setup changed ambient context before client setup finished")
					}
					if last == "client" {
						fmt.Fprintln(out, "matching-client-exports")
					}
					return nil
				})
			if err != nil || last != "client" {
				t.Fatalf("setup failed before final handoff: last=%s, %v\n%s", last, err, &output)
			}
			after, err := os.ReadFile(destination)
			if err != nil {
				t.Fatal(err)
			}
			text := output.String()
			if mode == "accept" || mode == "automation" {
				if bytes.Equal(before, after) || !strings.Contains(text, "export KUBECONFIG="+shellQuote(destination)) {
					t.Fatal("accepted handoff did not select the context or print the correct export")
				}
			} else if !bytes.Equal(before, after) {
				t.Fatal("setup changed kubeconfig without consent")
			}
			if mode == "automation" {
				if strings.Contains(text, "Import and select this kube-context?") {
					t.Fatal("--yes prompted for a context change")
				}
				client := strings.Index(text, "matching-client-exports")
				selected := strings.Index(text, "# Selected context")
				complete := strings.Index(text, "Setup complete.")
				demo := strings.Index(text, "# Try the counter demo:")
				if selected < client || complete < selected || demo < complete || !strings.Contains(text, "previous") {
					t.Fatalf("automatic handoff did not precede completion and demo instructions:\n%s", text)
				}
			} else {
				client := strings.Index(text, "matching-client-exports")
				prompt := strings.Index(text, "Import and select this kube-context?")
				complete := strings.Index(text, "Setup complete.")
				demo := strings.Index(text, "# Try the counter demo:")
				if prompt < client || complete < prompt || demo < complete || !strings.Contains(text, "previous") {
					t.Fatalf("handoff did not precede completion and demo instructions:\n%s", text)
				}
			}
		})
	}
}
