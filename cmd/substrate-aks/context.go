package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Azure/substrate-aks/internal/installer"
)

func offerContextSwitch(ctx context.Context, o setupOptions, c installer.Config, prompt *setupPrompt, ui *setupUI) (string, error) {
	if o.skipContextHandoff {
		ui.note("Kube-context handoff skipped (--skip-context-handoff). The final instructions use this installation's private kubeconfig.")
		return "", nil
	}
	selection, err := installer.PlanContextSwitch(o.contextFile)
	if err != nil {
		if o.yes {
			return "", fmt.Errorf("Substrate is installed, but context handoff is unavailable: %w\nChoose a valid --context-file PATH and retry: %s", err,
				cliCommand("client", "--config", o.path, "--use-context"))
		}
		ui.warning("Kube-context handoff unavailable: " + err.Error())
		ui.note("Your kubeconfig has not been changed. The final instructions will use this installation's private kubeconfig.")
		return "", nil
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
	if !o.yes {
		accepted, err := prompt.confirm(ctx, "Import and select this kube-context?", false)
		if errors.Is(err, io.EOF) {
			ui.note("No handoff consent received; kubeconfig unchanged. The final instructions will use this installation's private kubeconfig.")
			return "", nil
		}
		if err != nil || !accepted {
			return "", err
		}
	}
	if err := (installer.App{Config: c}).SelectContext(ctx, selection); err != nil {
		return "", fmt.Errorf("Substrate is installed, but context handoff failed: %w\nPrivate kubeconfig is unchanged. To use it without merging: %s\nTo retry importing, choose an unused --context-file PATH or resolve the reported destination problem first", err,
			cliCommand("client", "--config", o.path))
	}
	return selection.Path, nil
}
