package main

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Azure/substrate-aks/internal/installer"
)

// slowestSetupSteps is how many steps the console summary names.
const slowestSetupSteps = 3

// setupTimingRun records one guided setup run and saves its report.
type setupTimingRun struct {
	timings *installer.Timings
	// config is the installation whose logs directory receives the report;
	// it is set when stages begin, so setup that stops earlier saves none.
	config *installer.Config
	// receipt prints the summary after a successful run.
	receipt *setupUI
}

func newSetupTimingRun(now func() time.Time) *setupTimingRun {
	if now == nil {
		now = time.Now
	}
	timings := installer.NewTimings(now)
	timings.Phase("prepare", setupPhaseTitle("prepare"))
	return &setupTimingRun{timings: timings}
}

// setupStageID names a fixed setup stage, for example images.check.
func setupStageID(command string, flags []string) string {
	parts := []string{command}
	for _, flag := range flags {
		parts = append(parts, strings.ReplaceAll(strings.TrimPrefix(flag, "--"), "-", "_"))
	}
	return strings.Join(parts, ".")
}

// finish saves the report and returns the setup result. A report that cannot
// be saved is mentioned, but never replaces the result.
func (r *setupTimingRun) finish(err error) error {
	if r.config == nil {
		return err
	}
	report := r.timings.Report(installer.TimingOutcome(err))
	path, writeErr := installer.WriteTimingReport(*r.config, report)
	if err != nil {
		if writeErr != nil {
			return fmt.Errorf("%w\n\nSetup timings were not saved: %v", err, writeErr)
		}
		return fmt.Errorf("%w\n\nSetup timings: %s", err, path)
	}
	if r.receipt != nil {
		printSetupTimings(r.receipt, report, path, writeErr)
	}
	return nil
}

// printSetupTimings summarizes the report: each phase, marked with the phases
// it overlapped, the total and the slowest steps.
func printSetupTimings(u *setupUI, report installer.TimingReport, path string, writeErr error) {
	u.section("# Setup timings (wall / active):")
	var table strings.Builder
	w := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	for _, phase := range report.Phases {
		// A phase that ran alongside later phases names them and the time
		// it shared with them; without that time, the phases sum to the
		// total.
		var alongside string
		if len(phase.ConcurrentWith) > 0 {
			titles := make([]string, len(phase.ConcurrentWith))
			for i, id := range phase.ConcurrentWith {
				titles[i] = setupPhaseTitle(id)
			}
			alongside = fmt.Sprintf(" (%s alongside %s)", setupDuration(phase.Overlap()), strings.Join(titles, ", "))
		}
		fmt.Fprintf(w, "%s\t%s / %s%s\n", phase.Title, setupDuration(phase.Wall()), setupDuration(phase.Active()), alongside)
	}
	fmt.Fprintf(w, "Total\t%s / %s\n", setupDuration(report.Wall()), setupDuration(report.Active()))
	steps := report.Steps()
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].WallMS > steps[j].WallMS })
	if len(steps) > 0 {
		fmt.Fprintln(w, "Slowest steps:\t")
	}
	for _, step := range steps[:min(len(steps), slowestSetupSteps)] {
		fmt.Fprintf(w, "  %s\t%s\n", step.ID, setupDuration(step.Wall()))
	}
	w.Flush()
	summary := strings.TrimSuffix(table.String(), "\n")
	if writeErr == nil {
		// Paths are not wrapped, so they stay copyable.
		summary += "\nReport: " + setupDisplayPath(path)
	}
	u.command(summary)
	if writeErr != nil {
		u.warning(fmt.Sprintf("Setup timings were not saved: %v", writeErr))
	}
}
