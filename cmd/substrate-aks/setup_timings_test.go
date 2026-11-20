package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/substrate-aks/internal/installer"
)

type setupClock struct {
	mu  sync.Mutex
	now time.Time
}

func newSetupClock() *setupClock {
	return &setupClock{now: time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC)}
}

func (c *setupClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *setupClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// setupTimingReports returns the timing reports saved under home.
func setupTimingReports(t *testing.T, home string) []installer.TimingReport {
	t.Helper()
	var reports []installer.TimingReport
	err := filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !strings.HasSuffix(path, "-setup-timings.json") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var r installer.TimingReport
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		reports = append(reports, r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return reports
}

func TestSetupStageIDsAreValidTimingIDs(t *testing.T) {
	seen := map[string]bool{}
	for _, images := range []installer.ImagesConfig{{}, {Repository: "example.invalid/substrate"}} {
		for _, stage := range setupStages(installer.Config{Substrate: installer.SubstrateConfig{Images: images}}, true) {
			id := setupStageID(stage.command, stage.flags)
			timings := installer.NewTimings(time.Now)
			timings.Stage(id, stage.label)(nil)
			if got := timings.Report(installer.TimingSucceeded).Phases[0].Stages[0].ID; got != id {
				t.Fatalf("stage %q has an invalid timing ID %q", stage.label, id)
			}
			seen[id] = true
		}
	}
	for _, want := range []string{"plan.registry_only", "provision.registry_only", "images", "images.check", "client"} {
		if !seen[want] {
			t.Fatalf("missing stage ID %s in %v", want, seen)
		}
	}
}

func TestSetupRecordsStageTimings(t *testing.T) {
	path := testCLIConfig(t)
	clock := newSetupClock()
	var output bytes.Buffer
	publications := 0
	err := setupForTest(t.Context(), setupOptions{
		home: recordHome(path), name: recordName(path), containerEngine: "podman", withDemo: true,
		discovery: subscriptionDisplayRunner(t), now: clock.Now,
	}, strings.NewReader("yes\nyes\nyes\nyes\n"), &output, &output,
		func(ctx context.Context, args []string, _, _ io.Writer) error {
			if installer.TimingsFrom(ctx) == nil {
				t.Fatal("a stage command did not receive the setup recorder")
			}
			clock.Advance(time.Minute)
			if args[0] == "images" && !slices.Contains(args, "--check") {
				if publications++; publications == 1 {
					return errors.New("temporary publication failure")
				}
			}
			return nil
		})
	if err != nil {
		t.Fatalf("setup failed: %v\n%s", err, &output)
	}
	reports := setupTimingReports(t, recordHome(path))
	if len(reports) != 1 {
		t.Fatalf("setup saved %d timing reports", len(reports))
	}
	r := reports[0]
	var phases, stages []string
	for _, phase := range r.Phases {
		phases = append(phases, phase.ID)
		for _, stage := range phase.Stages {
			stages = append(stages, fmt.Sprintf("%s#%d:%s", stage.ID, stage.Attempt, stage.Outcome))
			if stage.Wall() != time.Minute || stage.Active() != time.Minute {
				t.Fatalf("stage %s measured %v / %v", stage.ID, stage.Wall(), stage.Active())
			}
		}
	}
	if r.Outcome != installer.TimingSucceeded || r.Wall() != 13*time.Minute ||
		!slices.Equal(phases, []string{"prepare", "images", "cluster", "substrate", "demo", "connect"}) {
		t.Fatalf("unexpected report: %s %v %v", r.Outcome, r.Wall(), phases)
	}
	wantStages := []string{
		"plan.registry_only#1:succeeded", "provision.registry_only#1:succeeded", "images#1:failed", "images#2:succeeded",
		"images.check#1:succeeded", "plan#1:succeeded", "provision#1:succeeded", "check#1:succeeded",
		"prepare#1:succeeded", "install#1:succeeded", "status#1:succeeded", "demo#1:succeeded", "client#1:succeeded",
	}
	if !slices.Equal(stages, wantStages) {
		t.Fatalf("stages were recorded as %v", stages)
	}
	if r.Phases[1].Title != "Images and assets" || r.Phases[1].Wall() != 5*time.Minute {
		t.Fatalf("images phase: %+v", r.Phases[1].TimingInterval)
	}
	text := output.String()
	teardown := strings.Index(text, "# Delete all resources")
	summary := strings.Index(text, "# Setup timings (wall / active):")
	if teardown < 0 || summary < teardown || !strings.Contains(text, "Images and assets") ||
		!strings.Contains(text, "Total") || !strings.Contains(text, "13:00 / 13:00") ||
		!strings.Contains(text, "-setup-timings.json") || strings.Count(text, "DONE  Setup complete.") != 1 {
		t.Fatalf("setup did not summarize timings after the receipt:\n%s", text)
	}
}

func TestSetupTimingsOnFailureKeepTheResult(t *testing.T) {
	for _, unwritable := range []bool{false, true} {
		t.Run(fmt.Sprint("unwritable=", unwritable), func(t *testing.T) {
			path := testCLIConfig(t)
			c, err := installer.LoadInstallation(recordHome(path), recordName(path))
			if err != nil {
				t.Fatal(err)
			}
			if unwritable {
				if err := os.WriteFile(filepath.Join(c.Installation.Dir, "logs"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			want := errors.New("install failed")
			var output bytes.Buffer
			err = setupForTest(t.Context(), setupOptions{
				home: recordHome(path), name: recordName(path), yes: true, discovery: subscriptionDisplayRunner(t),
			}, strings.NewReader(""), &output, &output,
				func(_ context.Context, args []string, _, _ io.Writer) error {
					if args[0] == "install" {
						return want
					}
					return nil
				})
			if !errors.Is(err, want) || !strings.Contains(err.Error(), "Resources are retained") {
				t.Fatalf("timing changed the setup result: %v", err)
			}
			reports := setupTimingReports(t, recordHome(path))
			if unwritable {
				if len(reports) != 0 || !strings.Contains(err.Error(), "Setup timings were not saved:") {
					t.Fatalf("an unsaved report was not explained: %v", err)
				}
				return
			}
			if len(reports) != 1 || !strings.Contains(err.Error(), "\n\nSetup timings: "+filepath.Join(c.Installation.Dir, "logs")) {
				t.Fatalf("failed setup did not point at its timing report: %d reports, %v", len(reports), err)
			}
			r := reports[0]
			last := r.Phases[len(r.Phases)-1]
			if r.Outcome != installer.TimingFailed || last.ID != "substrate" || last.Stages[len(last.Stages)-1].Outcome != installer.TimingFailed {
				t.Fatalf("failure was not recorded: %+v", r)
			}
		})
	}
}

func TestSetupStopsBeforeStagesWithoutAReport(t *testing.T) {
	path := testCLIConfig(t)
	var output bytes.Buffer
	err := setupForTest(t.Context(), setupOptions{
		home: recordHome(path), name: recordName(path), discovery: subscriptionDisplayRunner(t),
	}, strings.NewReader("no\n"), &output, &output,
		func(context.Context, []string, io.Writer, io.Writer) error {
			t.Fatal("declined setup ran a stage")
			return nil
		})
	if err == nil || strings.Contains(err.Error(), "Setup timings") {
		t.Fatalf("declined setup mentioned timings: %v", err)
	}
	if reports := setupTimingReports(t, recordHome(path)); len(reports) != 0 {
		t.Fatalf("declined setup saved %d timing reports", len(reports))
	}
}

// clockReader advances the clock whenever the prompt reads an answer.
type clockReader struct {
	io.Reader
	clock *setupClock
}

func (r clockReader) Read(p []byte) (int, error) {
	r.clock.Advance(time.Hour)
	return r.Reader.Read(p)
}

func TestSetupPromptsCountAsWaiting(t *testing.T) {
	clock := newSetupClock()
	timings := installer.NewTimings(clock.Now)
	ctx := installer.WithTimings(t.Context(), timings)
	timings.Phase("cluster", "AKS cluster")
	stop := timings.Stage("plan", "Preview full infrastructure")
	clock.Advance(time.Minute)
	var output bytes.Buffer
	prompt := newSetupPrompt(clockReader{strings.NewReader("yes\n"), clock}, newSetupUI(&output), true)
	accepted, err := prompt.confirm(ctx, "Apply these planned changes?", false)
	if err != nil || !accepted {
		t.Fatalf("prompt failed: %v, %v", accepted, err)
	}
	stop(nil)
	stage := timings.Report(installer.TimingSucceeded).Phases[0].Stages[0]
	if stage.Wall() < time.Hour || stage.Active() != time.Minute {
		t.Fatalf("answering a prompt counted as setup work: wall %v, active %v", stage.Wall(), stage.Active())
	}
}
