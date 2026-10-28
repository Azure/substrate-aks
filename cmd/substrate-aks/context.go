package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Azure/substrate-aks/internal/installer"
)

func offerContextSwitch(ctx context.Context, o setupOptions, c installer.Config, prompt *setupPrompt, ui *setupUI, out io.Writer) (bool, error) {
	if o.yes {
		return false, nil
	}
	selection, err := installer.PlanContextSwitch(o.contextFile)
	if err != nil {
		ui.warning("Kube-context handoff unavailable: " + err.Error())
		ui.note("Your kubeconfig has not been changed. Use the printed private KUBECONFIG export.")
		return false, nil
	}
	ui.section("Kubernetes context handoff")
	previous := selection.Previous
	if previous == "" {
		previous = "(none)"
	}
	ui.row("Destination", selection.Path)
	ui.row("Previous context", previous)
	ui.row("New context", c.AKS.ClusterName)
	ui.note("Import this installation and select its context. Unrelated entries are preserved; conflicting names are never overwritten.")
	accepted, err := prompt.confirm(ctx, "Import and select this kube-context?", false)
	if errors.Is(err, io.EOF) {
		ui.note("No handoff consent received; kubeconfig unchanged. Use the printed private KUBECONFIG export.")
		return false, nil
	}
	if err != nil || !accepted {
		return false, err
	}
	if err := (installer.App{Config: c}).SelectContext(ctx, selection); err != nil {
		return false, fmt.Errorf("Substrate is installed, but context handoff failed: %w\nPrivate kubeconfig is unchanged. Retry client handoff: %s", err,
			cliCommand("client", "--config", o.path, "--use-context", "--context-file", selection.Path))
	}
	return true, printSelectedContext(out, selection.Path, c.AKS.ClusterName)
}

func printSelectedContext(out io.Writer, path, context string) error {
	_, err := fmt.Fprintf(out, "\n# Selected context %s in %s.\n# Use this export instead of the earlier private-file export:\nexport KUBECONFIG=%s\nkubectl --context %s get pods --all-namespaces\n",
		shellQuote(context), shellQuote(path), shellQuote(path), shellQuote(context))
	return err
}
