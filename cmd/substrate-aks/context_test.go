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

func TestSetupContextHandoffRequiresFinalExplicitConsent(t *testing.T) {
	for _, mode := range []string{"accept", "decline", "EOF", "automation"} {
		t.Run(mode, func(t *testing.T) {
			path := testCLIConfig(t)
			c, err := installer.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
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
			err = setupForTest(t.Context(), setupOptions{
				path: path, yes: mode == "automation", withDemo: true,
				contextFile: destination, discovery: subscriptionDisplayRunner(t),
			}, strings.NewReader(answers), &output, &output,
				func(_ context.Context, args []string, out, _ io.Writer) error {
					last = args[0]
					current, err := os.ReadFile(destination)
					if err != nil || !bytes.Equal(current, before) {
						t.Fatal("setup changed ambient context before final consent")
					}
					if last == "client" {
						fmt.Fprintln(out, "matching-client-exports")
						if mode != "accept" {
							return nil
						}
						config := map[string]any{
							"apiVersion": "v1", "kind": "Config", "current-context": c.AKS.ClusterName,
							"contexts": []any{map[string]any{"name": c.AKS.ClusterName, "context": map[string]any{"cluster": "aks", "user": "operator"}}},
							"clusters": []any{map[string]any{"name": "aks", "cluster": map[string]any{"server": "https://aks.example.invalid"}}},
							"users":    []any{map[string]any{"name": "operator", "user": map[string]any{"token": "synthetic-token"}}},
						}
						data, err := json.Marshal(config)
						if err != nil {
							t.Fatal(err)
						}
						dir := filepath.Join(c.Installation.StateDir, c.Installation.ID)
						if err := os.MkdirAll(dir, 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(dir, "kubeconfig"), data, 0600); err != nil {
							t.Fatal(err)
						}
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
			if mode == "accept" {
				if bytes.Equal(before, after) || !strings.Contains(text, "export KUBECONFIG="+shellQuote(destination)) {
					t.Fatal("accepted handoff did not select the context or print the correct export")
				}
			} else if !bytes.Equal(before, after) {
				t.Fatal("setup changed kubeconfig without consent")
			}
			if mode == "automation" {
				if strings.Contains(text, "Import and select this kube-context?") {
					t.Fatal("--yes prompted for or opted into a context change")
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
