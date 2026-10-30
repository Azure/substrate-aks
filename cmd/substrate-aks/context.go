package main

import (
	"context"
	"errors"
	"io"

	"github.com/Azure/substrate-aks/internal/installer"
)

// offerContextSwitch returns the kubeconfig that now selects the installation,
// or, when an optional handoff problem left it unchanged, the warning lines
// shown so the caller can repeat them where they outlive the workspace.
func offerContextSwitch(ctx context.Context, o setupOptions, c installer.Config, prompt *setupPrompt, ui *setupUI) (string, []string, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	warn := func(lines ...string) (string, []string, error) {
		ui.warning(lines[0])
		for _, line := range lines[1:] {
			ui.note(line)
		}
		return "", lines, nil
	}
	if o.skipContextHandoff {
		ui.note("Kube-context handoff skipped (--skip-context-handoff). The final instructions use this installation's private kubeconfig.")
		return "", nil, nil
	}
	selection, err := installer.PlanContextSwitch(o.contextFile)
	if err != nil {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		return warn("Kube-context handoff unavailable: "+err.Error(),
			"Your kubeconfig has not been changed. The final instructions will use this installation's private kubeconfig.",
			"To retry importing, choose an unused --context-file PATH or resolve the reported destination problem first.")
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
			return "", nil, nil
		}
		if err != nil || !accepted {
			return "", nil, err
		}
	}
	if err := (installer.App{Config: c}).SelectContext(ctx, selection); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", nil, err
		}
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		return warn("Kube-context handoff failed: "+err.Error(),
			"The final instructions will use this installation's private kubeconfig.",
			"To retry importing, choose an unused --context-file PATH or resolve the reported destination problem first.")
	}
	return selection.Path, nil, nil
}
