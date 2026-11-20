package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manual clock for timing tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 20, 8, 0, 0, 0, time.FixedZone("PST", -8*3600))}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestTimingsSeparateWallAndActiveTime(t *testing.T) {
	clock := newFakeClock()
	timings := NewTimings(clock.Now)
	timings.Phase("prepare", "Preparation")
	clock.Advance(time.Second)
	// A wait that starts before images and ends inside it.
	endWait := timings.Wait()
	clock.Advance(2 * time.Second)
	timings.Phase("images", "Images and assets")
	clock.Advance(3 * time.Second)
	endWait()
	stopStage := timings.Stage("images", "Publish images")
	stopStep := timings.Step("images.imports")
	clock.Advance(4 * time.Second)
	// Overlapping waits count once.
	endA := timings.Wait()
	clock.Advance(time.Second)
	endB := timings.Wait()
	clock.Advance(time.Second)
	endA()
	clock.Advance(time.Second)
	endB()
	stopStep(nil)
	stopStage(nil)
	clock.Advance(5 * time.Second)
	// A wait outside every stage counts against the phase only.
	endOutside := timings.Wait()
	clock.Advance(6 * time.Second)
	endOutside()

	r := timings.Report(TimingSucceeded)
	if r.Version != TimingReportVersion || r.Outcome != TimingSucceeded || len(r.Phases) != 2 {
		t.Fatalf("unexpected report: %+v", r)
	}
	want := []struct {
		name         string
		got          TimingInterval
		wall, active time.Duration
	}{
		{"total", r.TimingInterval, 24 * time.Second, 10 * time.Second},
		{"prepare", r.Phases[0].TimingInterval, 3 * time.Second, time.Second},
		{"images", r.Phases[1].TimingInterval, 21 * time.Second, 9 * time.Second},
		{"stage", r.Phases[1].Stages[0].TimingInterval, 7 * time.Second, 4 * time.Second},
		{"step", r.Phases[1].Stages[0].Steps[0].TimingInterval, 7 * time.Second, 4 * time.Second},
	}
	for _, w := range want {
		if w.got.Wall() != w.wall || w.got.Active() != w.active {
			t.Errorf("%s: wall %v active %v, want %v and %v", w.name, w.got.Wall(), w.got.Active(), w.wall, w.active)
		}
	}
	if r.Started != "2026-11-20T16:00:00.000Z" || r.Phases[1].Started != "2026-11-20T16:00:03.000Z" || r.Ended != "2026-11-20T16:00:24.000Z" {
		t.Fatalf("stamps are not UTC with milliseconds: %s, %s, %s", r.Started, r.Phases[1].Started, r.Ended)
	}
}

func TestTimingsNilRecorderIsNoOp(t *testing.T) {
	var timings *Timings
	timings.Phase("images", "Images")
	timings.Stage("images", "Publish")(errors.New("x"))
	timings.Step("images.imports")(nil)
	timings.StepWorkers("images.imports", 4)(nil)
	timings.Wait()()
	if r := timings.Report(TimingSucceeded); r.Version != 0 || len(r.Phases) != 0 {
		t.Fatalf("nil recorder reported: %+v", r)
	}
	if TimingsFrom(t.Context()) != nil {
		t.Fatal("a context without a recorder returned one")
	}
	stop := (App{}).step(t.Context(), "images.imports")
	stop(nil)
}

func TestTimingsStageAttemptsAndOutcomes(t *testing.T) {
	clock := newFakeClock()
	timings := NewTimings(clock.Now)
	ctx := WithTimings(t.Context(), timings)
	if TimingsFrom(ctx) != timings {
		t.Fatal("the context lost the recorder")
	}
	timings.Phase("images", "Images and assets")
	stop := timings.Stage("images", "Publish images")
	clock.Advance(time.Second)
	stop(errors.New("temporary"))
	stop(nil) // a second stop keeps the first outcome
	stop = timings.Stage("images", "Publish images")
	App{}.step(ctx, "images.imports")(fmt.Errorf("wrapped: %w", context.Canceled))
	stop(nil)
	timings.Phase("images", "ignored: phase unchanged")
	timings.Phase("cluster", "AKS cluster")
	timings.Stage("provision", "Provision AKS")
	timings.Step("infrastructure.deployment")
	clock.Advance(time.Second)
	r := timings.Report(TimingOutcome(context.Canceled))
	if r.Outcome != TimingInterrupted || len(r.Phases) != 2 || r.Phases[0].Title != "Images and assets" {
		t.Fatalf("unexpected report: %+v", r)
	}
	stages := r.Phases[0].Stages
	if len(stages) != 2 || stages[0].Attempt != 1 || stages[0].Outcome != TimingFailed ||
		stages[1].Attempt != 2 || stages[1].Outcome != TimingSucceeded {
		t.Fatalf("attempts were not recorded separately: %+v", stages)
	}
	if steps := stages[1].Steps; len(steps) != 1 || steps[0].Outcome != TimingInterrupted || len(stages[0].Steps) != 0 {
		t.Fatalf("a step belongs to the wrong attempt or lost cancellation: %+v", stages)
	}
	open := r.Phases[1].Stages[0]
	if open.Outcome != TimingUnfinished || open.Steps[0].Outcome != TimingUnfinished || open.Wall() != time.Second || open.Ended != r.Ended {
		t.Fatalf("running work was not reported as unfinished at report time: %+v", open)
	}
}

func TestTimingsStepsOutsideStagesAndOrdering(t *testing.T) {
	clock := newFakeClock()
	timings := NewTimings(clock.Now)
	// No phase started: steps land in an implicit setup phase.
	stopB := timings.Step("b.step")
	stopA := timings.Step("a.step")
	clock.Advance(time.Second)
	stopLate := timings.StepWorkers("images.imports", 4)
	clock.Advance(time.Second)
	stopLate(nil)
	stopA(nil)
	stopB(nil)
	r := timings.Report(TimingSucceeded)
	if len(r.Phases) != 1 || r.Phases[0].ID != "setup" {
		t.Fatalf("steps outside a phase were not kept: %+v", r.Phases)
	}
	var ids []string
	for _, s := range r.Steps() {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, ",") != "a.step,b.step,images.imports" || r.Steps()[2].Workers != 4 {
		t.Fatalf("steps are not sorted by start, then ID: %v", r.Steps())
	}
}

func TestTimingsConcurrentSteps(t *testing.T) {
	timings := NewTimings(time.Now)
	timings.Phase("images", "Images and assets")
	stop := timings.Stage("images", "Publish images")
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			end := timings.Step(fmt.Sprintf("images.import.image-%02d", i))
			wait := timings.Wait()
			wait()
			end(nil)
		})
	}
	wg.Wait()
	stop(nil)
	if steps := timings.Report(TimingSucceeded).Steps(); len(steps) != 32 {
		t.Fatalf("concurrent steps were lost: %d", len(steps))
	}
}

func TestTimingsRejectUnsafeIdentifiers(t *testing.T) {
	timings := NewTimings(newFakeClock().Now)
	for _, id := range []string{"images.import.ateapi", "rollout.demo.counter-microvm", "provision.registry_only"} {
		if timingID(id) != id {
			t.Fatalf("valid ID %q was replaced", id)
		}
	}
	for _, id := range []string{"", "Images", "images./subscriptions/x", "images.import.a b", "images..x", "images.import.substratetest123.azurecr.io/x", "x\n"} {
		timings.Step(id)(nil)
		if timingID(id) != invalidTimingID {
			t.Fatalf("unsafe ID %q was kept", id)
		}
	}
	for _, s := range timings.Report(TimingSucceeded).Steps() {
		if s.ID != invalidTimingID {
			t.Fatalf("unsafe ID reached the report: %q", s.ID)
		}
	}
}

func TestWriteTimingReport(t *testing.T) {
	c := testConfig(t)
	clock := newFakeClock()
	timings := NewTimings(clock.Now)
	timings.Phase("images", "Images and assets")
	clock.Advance(1500 * time.Millisecond)
	report := timings.Report(TimingSucceeded)
	path, err := WriteTimingReport(c, report)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != filepath.Join(c.workspace(), "logs") || filepath.Base(path) != "20261120T160001.500000000-setup-timings.json" {
		t.Fatalf("report written to %s", path)
	}
	for file, mode := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700 | os.ModeDir} {
		info, err := os.Stat(file)
		if err != nil || info.Mode() != mode {
			t.Fatalf("%s has mode %v, want %v: %v", file, info.Mode(), mode, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded TimingReport
	if err := json.Unmarshal(data, &decoded); err != nil || decoded.Phases[0].WallMS != 1500 || decoded.Version != TimingReportVersion {
		t.Fatalf("report did not round-trip: %v\n%s", err, data)
	}
	if _, err := WriteTimingReport(c, report); err == nil {
		t.Fatal("a second report overwrote the first")
	}

	missing := testConfig(t)
	missing.Installation.Dir = filepath.Join(t.TempDir(), "discarded")
	if _, err := WriteTimingReport(missing, report); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("writing for a missing installation returned %v", err)
	}
	if _, err := os.Stat(missing.workspace()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("writing a report created the installation directory")
	}
}

var reportGUID = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// TestTimingReportOmitsPrivateIdentifiers drives real installer operations,
// including an Azure failure whose error names private resources, and checks
// that the saved report holds none of them.
func TestTimingReportOmitsPrivateIdentifiers(t *testing.T) {
	c := testConfig(t)
	c.Substrate.Images = ImagesConfig{}
	c.Installation.Name = "private-installation-name"
	c.Azure.ResourceGroup = "private-resource-group"
	t.Setenv("PATH", "")
	timings := NewTimings(time.Now)
	ctx := WithTimings(t.Context(), timings)
	timings.Phase("images", "Images and assets")
	stop := timings.Stage("images", "Publish images and prepare runtime assets")
	replies := []reply{accountReply(c), {contains: "group exists", output: "true"}, ownerReply(c), registryReply(c), acrLoginReply()}
	replies = append(replies, fetchReplies(c.Substrate.Source)...)
	replies = append(replies, releaseImportReplies(t, c, func(image releaseImage) string { return image.Digest })...)
	replies = append(replies, extraImageReplies(c, c.Substrate.Source.Tag)...)
	r := script(t, replies...)
	assetsDir := filepath.Join(c.workspace(), "assets")
	if _, err := (App{Config: c, Runner: imageBuildRunner{r, assetsDir}, ContainerEngine: "docker"}).BuildImages(ctx); err != nil {
		t.Fatal(err)
	}
	stop(nil)

	// Error text and identifiers that must never reach the report.
	const token = "eyJ0eXAiOiJKV1QiLCJhbGciOiJSUzI1NiJ9.secret-token"
	secrets := []string{
		c.Azure.SubscriptionID, c.Azure.AssetPublisherObjectID, c.Installation.ID, c.Installation.Name,
		c.Azure.ResourceGroup, "substratetest123", c.workspace(), token,
		"/subscriptions/" + c.Azure.SubscriptionID + "/resourceGroups/" + c.Azure.ResourceGroup,
	}
	timings.Phase("cluster", "AKS cluster")
	stop = timings.Stage("plan", "Preview full infrastructure")
	failure := fmt.Errorf("what-if failed: %s", strings.Join(secrets, " "))
	r = script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "false"},
		reply{contains: "deployment sub what-if", err: failure})
	err := (App{Config: c, Runner: r}).Provision(ctx, true)
	if !errors.Is(err, failure) {
		t.Fatalf("the scripted what-if did not fail: %v", err)
	}
	stop(err)

	path, err := WriteTimingReport(c, timings.Report(TimingOutcome(err)))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	report := string(data)
	for _, secret := range secrets {
		if strings.Contains(report, secret) {
			t.Fatalf("report contains %q:\n%s", secret, report)
		}
	}
	for _, leak := range []string{"/subscriptions/", ".azurecr.io", "what-if failed", "eyJ"} {
		if strings.Contains(report, leak) {
			t.Fatalf("report contains %q:\n%s", leak, report)
		}
	}
	if reportGUID.MatchString(report) {
		t.Fatalf("report contains a GUID:\n%s", report)
	}
	var decoded TimingReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range decoded.Steps() {
		ids = append(ids, s.ID)
	}
	for _, want := range []string{"images.registry", "images.release_inventory", "images.imports", "images.import.ateapi", "images.verify.envoy-dataplane", "images.assets_download", "infrastructure.what_if"} {
		if !strings.Contains(","+strings.Join(ids, ",")+",", ","+want+",") {
			t.Fatalf("report is missing step %s: %v", want, ids)
		}
	}
	for _, id := range ids {
		if id == invalidTimingID {
			t.Fatalf("an installer step ID is invalid: %v", ids)
		}
	}
	if outcome := decoded.Phases[1].Stages[0].Steps[0].Outcome; outcome != TimingFailed || decoded.Outcome != TimingFailed {
		t.Fatalf("the failed what-if was recorded as %s", outcome)
	}
}
