package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/substrate-aks/internal/installer"
	"github.com/charmbracelet/x/ansi"
)

// stageLog records stage commands run by the foreground and the background.
type stageLog struct {
	mu       sync.Mutex
	commands []string
}

// add records args as a stage ID such as provision.registry_only and returns
// that ID.
func (l *stageLog) add(args []string) string {
	var flags []string
	for _, arg := range args[2:] {
		if arg == "--check" || arg == "--registry-only" {
			flags = append(flags, arg)
		}
	}
	id := setupStageID(args[0], flags)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.commands = append(l.commands, id)
	return id
}

func (l *stageLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.commands)
}

// foreground drops the background images stage.
func foreground(commands []string) []string {
	return slices.DeleteFunc(slices.Clone(commands), func(id string) bool { return id == "images" })
}

func publishing(args []string) bool {
	return args[0] == "images" && !slices.Contains(args, "--check")
}

// waitFor fails a fake stage that would otherwise block forever, for example
// because setup ran the stages it waits for serially.
func waitFor(ch <-chan struct{}, what string) error {
	select {
	case <-ch:
		return nil
	case <-time.After(30 * time.Second):
		return fmt.Errorf("timed out waiting for %s", what)
	}
}

func overlapInstallation(t *testing.T) (string, installer.Config) {
	t.Helper()
	path := testCLIConfig(t)
	c, err := installer.LoadInstallation(recordHome(path), recordName(path))
	if err != nil {
		t.Fatal(err)
	}
	return path, c
}

// lockHeld checks that setup holds c's lock and ctx lets a stage share it.
func lockHeld(ctx context.Context, c installer.Config) error {
	if f, err := installer.Lock(c); !errors.Is(err, installer.ErrInstallationInUse) {
		if f != nil {
			f.Close()
		}
		return fmt.Errorf("setup did not hold the installation lock: %v", err)
	}
	shared, err := installer.LockFor(ctx, c)
	if err != nil {
		return fmt.Errorf("a stage could not share setup's lock: %w", err)
	}
	return shared.Close()
}

// lockFree checks that nothing holds c's lock.
func lockFree(c installer.Config) error {
	f, err := installer.Lock(c)
	if err != nil {
		return fmt.Errorf("the installation lock was not released: %w", err)
	}
	return f.Close()
}

func TestSetupStagesOverlapImagesOnlyForFreshInstallations(t *testing.T) {
	describe := func(stages []setupStage) []string {
		var out []string
		for _, s := range stages {
			id := setupStageID(s.command, s.flags) + "@" + s.phaseID()
			if s.background {
				id += "&"
			}
			if s.join {
				id += "|"
			}
			out = append(out, id)
		}
		return out
	}
	serial := []string{"plan.registry_only@images", "provision.registry_only@images", "images@images", "images.check@images",
		"plan@cluster", "provision@cluster", "check@cluster", "prepare@substrate", "install@substrate", "status@substrate", "client@connect"}
	resumed := slices.Delete(slices.Clone(serial), 0, 3)
	overlap := []string{"plan.registry_only@images", "provision.registry_only@images", "images@images&",
		"plan@cluster", "provision@cluster", "check@cluster", "images.check@substrate|", "prepare@substrate", "install@substrate", "status@substrate", "client@connect"}
	configured := installer.Config{Substrate: installer.SubstrateConfig{Images: installer.ImagesConfig{Repository: "example.invalid/substrate"}}}
	for _, tc := range []struct {
		name, env string
		config    installer.Config
		want      []string
	}{
		{"fresh", "", installer.Config{}, overlap},
		{"fresh serial", "1", installer.Config{}, serial},
		{"fresh with an unparsable setting", "yes please", installer.Config{}, overlap},
		{"resumed", "", configured, resumed},
		{"resumed serial", "true", configured, resumed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(setupSerialEnv, tc.env)
			if got := describe(setupStages(tc.config, false)); !slices.Equal(got, tc.want) {
				t.Fatalf("stages = %v, want %v", got, tc.want)
			}
			if got := setupPhases(tc.config, false); !slices.Equal(got, []string{"prepare", "images", "cluster", "substrate", "connect"}) {
				t.Fatalf("phases = %v", got)
			}
		})
	}
}

func TestSetupSerialSettingIsNeverSilent(t *testing.T) {
	for _, tc := range []struct {
		value  string
		serial bool
		note   string
	}{
		{"", false, ""},
		{"0", false, ""},
		{"1", true, "images publish and are verified before AKS is previewed"},
		{"yes", false, `SUBSTRATE_AKS_SERIAL_SETUP="yes" is not a Boolean`},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(setupSerialEnv, tc.value)
			serial, note := setupSerial()
			if serial != tc.serial || (tc.note == "") != (note == "") || !strings.Contains(note, tc.note) {
				t.Fatalf("setupSerial() = %v, %q", serial, note)
			}
		})
	}
	// Setup shows the note for a fresh installation.
	path, _ := overlapInstallation(t)
	t.Setenv(setupSerialEnv, "yes")
	var output bytes.Buffer
	err := setupForTest(t.Context(), setupOptions{
		home: recordHome(path), name: recordName(path), yes: true, skipContextHandoff: true, discovery: subscriptionDisplayRunner(t),
	}, strings.NewReader(""), &output, &output, func(context.Context, []string, io.Writer, io.Writer) error { return nil })
	if text := ansi.Strip(output.String()); err != nil || !strings.Contains(strings.Join(strings.Fields(text), " "), "is not a Boolean") {
		t.Fatalf("setup did not report the unparsable setting: %v\n%s", err, text)
	}
}

func TestSetupBackgroundTellsCancellationFromFailure(t *testing.T) {
	realErr := errors.New("[image a] unauthorized")
	interrupted := fmt.Errorf("az interrupted: %w", context.Canceled)
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"canceled", context.Canceled, true},
		{"deadline", context.DeadlineExceeded, true},
		{"wrapped", interrupted, true},
		{"joined cancellations", errors.Join(interrupted, fmt.Errorf("stopped: %w", context.DeadlineExceeded)), true},
		{"joined with a failure", errors.Join(realErr, interrupted), false},
		{"nested join with a failure", fmt.Errorf("imports: %w", errors.Join(interrupted, errors.Join(interrupted, realErr))), false},
		// A cancelled subprocess whose error does not wrap the context's.
		{"killed", errors.New("signal: killed"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cancellationOnly(tc.err); got != tc.want {
				t.Fatalf("cancellationOnly(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
	// An import failure joined with the imports its cancellation
	// interrupted is reported after the foreground failure that stopped
	// setup.
	t.Run("foreground failure", func(t *testing.T) {
		path, c := overlapInstallation(t)
		provisionErr := errors.New("AKS quota exceeded")
		var log stageLog
		err := setupForTest(t.Context(), setupOptions{
			home: recordHome(path), name: recordName(path), yes: true, skipContextHandoff: true, discovery: subscriptionDisplayRunner(t),
		}, strings.NewReader(""), io.Discard, io.Discard,
			func(ctx context.Context, args []string, _, _ io.Writer) error {
				switch log.add(args) {
				case "images":
					if err := waitFor(ctx.Done(), "cancellation"); err != nil {
						return err
					}
					return errors.Join(realErr, fmt.Errorf("importing release images stopped after 1 of 9: %w", ctx.Err()))
				case "provision":
					return provisionErr
				}
				return nil
			})
		if !errors.Is(err, provisionErr) || !errors.Is(err, realErr) || !strings.Contains(err.Error(), "also failed") {
			t.Fatalf("the image failure was dropped on cancellation: %v", err)
		}
		// The cancellation beside the import failure does not make the
		// run, or the images stage, an interruption.
		reports := setupTimingReports(t, recordHome(path))
		if len(reports) != 1 || reports[0].Outcome != installer.TimingFailed {
			t.Fatalf("a failed setup was not reported as failed: %+v", reports)
		}
		var images []string
		for _, phase := range reports[0].Phases {
			for _, stage := range phase.Stages {
				if stage.ID == "images" {
					images = append(images, stage.Outcome)
				}
			}
		}
		if !slices.Equal(images, []string{installer.TimingFailed}) {
			t.Fatalf("the images stage was not reported as failed: %v", images)
		}
		if err := lockFree(c); err != nil {
			t.Fatal(err)
		}
	})
	// A failure joined with a cancellation while setup still runs is
	// offered a retry like any other failure.
	t.Run("retry offered", func(t *testing.T) {
		path, c := overlapInstallation(t)
		var published <-chan struct{}
		provisioning := make(chan struct{})
		var output bytes.Buffer
		// Continue, no demo, apply the registry, apply the cluster, decline
		// the retry.
		err := setupForTest(t.Context(), setupOptions{
			home: recordHome(path), name: recordName(path), skipContextHandoff: true, discovery: subscriptionDisplayRunner(t),
			backgroundStarted: func(done <-chan struct{}) { published = done },
		}, strings.NewReader("yes\nno\nyes\nyes\nno\n"), &output, &output,
			func(_ context.Context, args []string, _, _ io.Writer) error {
				if publishing(args) {
					if err := waitFor(provisioning, "AKS provisioning"); err != nil {
						return err
					}
					return errors.Join(realErr, interrupted)
				}
				if args[0] == "provision" && !slices.Contains(args, "--registry-only") {
					close(provisioning)
					return waitFor(published, "image publishing")
				}
				return nil
			})
		if !errors.Is(err, realErr) || !strings.Contains(output.String(), "Retry this stage") {
			t.Fatalf("the image failure was not offered a retry: %v\n%s", err, &output)
		}
		if err := lockFree(c); err != nil {
			t.Fatal(err)
		}
	})
	// A stage that hits its own deadline while setup still runs, such as
	// a buildx bootstrap timing out, failed: it is offered a retry rather
	// than treated as setup being cancelled.
	t.Run("own deadline retry offered", func(t *testing.T) {
		path, c := overlapInstallation(t)
		deadline := errors.Join(fmt.Errorf("docker interrupted: %w", context.DeadlineExceeded))
		var published <-chan struct{}
		provisioning := make(chan struct{})
		var output bytes.Buffer
		// Continue, no demo, apply the registry, apply the cluster, decline
		// the retry.
		err := setupForTest(t.Context(), setupOptions{
			home: recordHome(path), name: recordName(path), skipContextHandoff: true, discovery: subscriptionDisplayRunner(t),
			backgroundStarted: func(done <-chan struct{}) { published = done },
		}, strings.NewReader("yes\nno\nyes\nyes\nno\n"), &output, &output,
			func(_ context.Context, args []string, _, _ io.Writer) error {
				if publishing(args) {
					if err := waitFor(provisioning, "AKS provisioning"); err != nil {
						return err
					}
					return deadline
				}
				if args[0] == "provision" && !slices.Contains(args, "--registry-only") {
					close(provisioning)
					return waitFor(published, "image publishing")
				}
				return nil
			})
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(output.String(), "Retry this stage") {
			t.Fatalf("the image stage deadline was not offered a retry: %v\n%s", err, &output)
		}
		if err := lockFree(c); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSetupOverlapsImagesWithCluster(t *testing.T) {
	path, c := overlapInstallation(t)
	clock := newSetupClock()
	var log stageLog
	provisioning := make(chan struct{})
	var output bytes.Buffer
	err := setupForTest(t.Context(), setupOptions{
		home: recordHome(path), name: recordName(path), yes: true, withDemo: true, skipContextHandoff: true,
		discovery: subscriptionDisplayRunner(t), now: clock.Now,
	}, strings.NewReader(""), &output, &output,
		func(ctx context.Context, args []string, out, stderr io.Writer) error {
			switch id := log.add(args); {
			case publishing(args):
				if setupSessionFrom(ctx) != nil {
					return errors.New("the background stage reported to the workspace")
				}
				if err := lockHeld(ctx, c); err != nil {
					return err
				}
				// Finishes only once AKS provisioning has started.
				if err := waitFor(provisioning, "AKS provisioning"); err != nil {
					return err
				}
				clock.Advance(time.Minute)
				fmt.Fprint(stderr, "[image a]\nimported a\n[image b]\nimported b\n")
				fmt.Fprintln(out, "Saved ACR image references.")
			case id == "provision":
				if err := lockHeld(ctx, c); err != nil {
					return err
				}
				clock.Advance(5 * time.Minute)
				close(provisioning)
			case id == "plan":
				if err := lockHeld(ctx, c); err != nil {
					return err
				}
			// Images may have been collected before check, which releases
			// the lock, so check asserts nothing about it.
			case id == "images.check" || id == "prepare":
				if err := lockFree(c); err != nil {
					return err
				}
			}
			return nil
		})
	if err != nil {
		t.Fatalf("setup failed: %v\n%s", err, &output)
	}
	commands := log.list()
	want := []string{"plan.registry_only", "provision.registry_only", "plan", "provision", "check", "images.check", "prepare", "install", "status", "demo", "client"}
	if got := foreground(commands); !slices.Equal(got, want) {
		t.Fatalf("foreground stages = %v, want %v", got, want)
	}
	if i := slices.Index(commands, "images"); i < 2 || slices.Index(commands[i+1:], "images") >= 0 {
		t.Fatalf("images did not publish once after the registry existed: %v", commands)
	}
	if err := lockFree(c); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "[image a]\nimported a\n[image b]\nimported b\n") ||
		strings.Index(text, "[image a]") > strings.Index(text, "Prepare Azure deployment manifests") {
		t.Fatalf("background output was not shown before Substrate preparation:\n%s", text)
	}
	reports := setupTimingReports(t, recordHome(path))
	if len(reports) != 1 {
		t.Fatalf("setup saved %d timing reports", len(reports))
	}
	r := reports[0]
	if r.Version != 2 {
		t.Fatalf("report version %d", r.Version)
	}
	var wall time.Duration
	for _, phase := range r.Phases {
		wall += phase.Wall() - phase.Overlap()
		if phase.ID != "images" {
			if len(phase.ConcurrentWith) > 0 || phase.OverlapMS != 0 {
				t.Fatalf("phase %s overlapped: %+v", phase.ID, phase)
			}
			continue
		}
		// Publishing may still run when Substrate starts waiting for it.
		if len(phase.ConcurrentWith) == 0 || phase.ConcurrentWith[0] != "cluster" || phase.Overlap() < 5*time.Minute {
			t.Fatalf("images phase did not record its overlap with the cluster: %+v", phase)
		}
		if n := len(phase.Stages); n != 3 || !phase.Stages[2].Background || phase.Stages[2].ID != "images" || phase.Stages[0].Background {
			t.Fatalf("images phase stages: %+v", phase.Stages)
		}
	}
	if wall != r.Wall() {
		t.Fatalf("phases count %v without overlap, setup took %v", wall, r.Wall())
	}
	if !strings.Contains(ansi.Strip(text), " alongside AKS cluster") {
		t.Fatalf("timing summary did not mark the overlap:\n%s", text)
	}
}

// signalWriter is a goroutine-safe writer that closes seen once its output
// contains text.
type signalWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	text string
	// count is how many times text must appear; zero means once.
	count int
	seen  chan struct{}
	once  sync.Once
}

func newSignalWriter(text string) *signalWriter {
	return &signalWriter{text: text, seen: make(chan struct{})}
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if strings.Count(w.buf.String(), w.text) >= max(w.count, 1) {
		w.once.Do(func() { close(w.seen) })
	}
	return n, err
}

func (w *signalWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestSetupTimesImageWaitInSubstrate checks that waiting for images that
// outlast AKS provisioning counts in the Substrate phase, which waits, and
// not in the AKS cluster phase, so no phase counts the same time twice.
func TestSetupTimesImageWaitInSubstrate(t *testing.T) {
	path, _ := overlapInstallation(t)
	clock := newSetupClock()
	output := newSignalWriter("Waiting for image publishing to finish")
	err := setupForTest(t.Context(), setupOptions{
		home: recordHome(path), name: recordName(path), yes: true, skipContextHandoff: true,
		discovery: subscriptionDisplayRunner(t), now: clock.Now,
	}, strings.NewReader(""), output, output,
		func(_ context.Context, args []string, _, _ io.Writer) error {
			switch {
			case publishing(args):
				// Publishing takes 10 minutes and finishes only after
				// Substrate has started waiting for it.
				if err := waitFor(output.seen, "the wait for images"); err != nil {
					return err
				}
				clock.Advance(7 * time.Minute)
			case args[0] == "provision" && !slices.Contains(args, "--registry-only"):
				clock.Advance(3 * time.Minute)
			}
			return nil
		})
	text := ansi.Strip(output.String())
	if err != nil {
		t.Fatalf("setup failed: %v\n%s", err, text)
	}
	if verify, wait := strings.Index(text, "Verify published images before installing Substrate"), strings.Index(text, "Waiting for image publishing"); verify < 0 || wait < verify {
		t.Fatalf("setup did not wait for images in the Substrate phase:\n%s", text)
	}
	reports := setupTimingReports(t, recordHome(path))
	if len(reports) != 1 {
		t.Fatalf("setup saved %d timing reports", len(reports))
	}
	r := reports[0]
	phases := map[string]installer.PhaseTimingReport{}
	var unique time.Duration
	for _, phase := range r.Phases {
		phases[phase.ID] = phase
		unique += phase.Wall() - phase.Overlap()
	}
	if r.Wall() != 10*time.Minute || unique != r.Wall() {
		t.Fatalf("setup took %v, phases count %v without overlap", r.Wall(), unique)
	}
	images, cluster, substrate := phases["images"], phases["cluster"], phases["substrate"]
	if images.Wall() != 10*time.Minute || images.Overlap() != 10*time.Minute ||
		!slices.Equal(images.ConcurrentWith, []string{"cluster", "substrate"}) {
		t.Fatalf("images phase: %+v", images)
	}
	var stages time.Duration
	for _, stage := range cluster.Stages {
		stages += stage.Wall()
	}
	if cluster.Wall() != 3*time.Minute || stages != cluster.Wall() || cluster.OverlapMS != 0 {
		t.Fatalf("AKS cluster phase counted the wait for images: wall %v, stages %v", cluster.Wall(), stages)
	}
	if substrate.Wall() != 7*time.Minute || substrate.Stages[0].ID != "images.check" || substrate.Stages[0].Wall() != 0 {
		t.Fatalf("Substrate phase did not count the wait for images: %+v", substrate)
	}
	for _, line := range []string{
		"Images and assets  10:00 / 10:00 (10:00 alongside AKS cluster, Substrate)",
		"AKS cluster        3:00 / 3:00\n",
		"Substrate          7:00 / 7:00\n",
		"Total              10:00 / 10:00\n",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("timing summary lacks %q:\n%s", line, text)
		}
	}
}

func TestSetupDeclinedApprovalCancelsImages(t *testing.T) {
	path, c := overlapInstallation(t)
	var log stageLog
	var exited atomic.Bool
	var output bytes.Buffer
	// Continue, no demo, apply the registry, decline the cluster.
	err := setupForTest(t.Context(), setupOptions{
		home: recordHome(path), name: recordName(path), skipContextHandoff: true, discovery: subscriptionDisplayRunner(t),
	}, strings.NewReader("yes\nno\nyes\nno\n"), &output, &output,
		func(ctx context.Context, args []string, _, _ io.Writer) error {
			log.add(args)
			if publishing(args) {
				defer exited.Store(true)
				if err := waitFor(ctx.Done(), "cancellation"); err != nil {
					return err
				}
				return fmt.Errorf("images interrupted: %w", ctx.Err())
			}
			return nil
		})
	if err == nil || !strings.Contains(err.Error(), "no deployment was submitted") {
		t.Fatalf("declined approval did not stop setup: %v", err)
	}
	if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "also failed") {
		t.Fatalf("the cancelled images stage was reported as a failure: %v", err)
	}
	if !exited.Load() {
		t.Fatal("setup returned while the images stage was still running")
	}
	if got := foreground(log.list()); !slices.Equal(got, []string{"plan.registry_only", "provision.registry_only", "plan"}) {
		t.Fatalf("stages after the declined approval: %v", got)
	}
	if err := lockFree(c); err != nil {
		t.Fatal(err)
	}
}

func TestSetupImageFailureStopsBeforeSubstrate(t *testing.T) {
	imagesErr := errors.New("2 of 9 release images failed:\n[image a] digest mismatch\n[image b] not found")
	t.Run("while AKS provisions", func(t *testing.T) {
		path, c := overlapInstallation(t)
		var log stageLog
		provisioning := make(chan struct{})
		err := setupForTest(t.Context(), setupOptions{
			home: recordHome(path), name: recordName(path), yes: true, skipContextHandoff: true, discovery: subscriptionDisplayRunner(t),
		}, strings.NewReader(""), io.Discard, io.Discard,
			func(_ context.Context, args []string, _, _ io.Writer) error {
				log.add(args)
				if publishing(args) {
					if err := waitFor(provisioning, "AKS provisioning"); err != nil {
						return err
					}
					return imagesErr
				}
				if args[0] == "provision" && !slices.Contains(args, "--registry-only") {
					close(provisioning)
				}
				return nil
			})
		if !errors.Is(err, imagesErr) || !strings.Contains(err.Error(), "setup stopped at Publish images and prepare runtime assets") ||
			!strings.Contains(err.Error(), "Retry failed stage: "+cliCommand("images", c.Installation.Name)) ||
			!strings.Contains(err.Error(), "Resources are retained") {
			t.Fatalf("image failure lost its errors or retry guidance: %v", err)
		}
		commands := log.list()
		if !slices.Contains(commands, "provision") || slices.Contains(commands, "images.check") || slices.Contains(commands, "prepare") {
			t.Fatalf("setup did not stop before Substrate: %v", commands)
		}
		if err := lockFree(c); err != nil {
			t.Fatal(err)
		}
	})
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprint("before the cluster approval, retry=", retry), func(t *testing.T) {
			path, _ := overlapInstallation(t)
			var log stageLog
			var published <-chan struct{}
			var publications atomic.Int32
			planning := make(chan struct{})
			var previews atomic.Int32
			var output bytes.Buffer
			// Continue, no demo, apply the registry, then answer the retry
			// offer before the cluster approval.
			answers := "yes\nno\nyes\nno\n"
			if retry {
				answers = "yes\nno\nyes\nyes\nyes\n"
			}
			err := setupForTest(t.Context(), setupOptions{
				home: recordHome(path), name: recordName(path), skipContextHandoff: true, discovery: subscriptionDisplayRunner(t),
				backgroundStarted: func(done <-chan struct{}) { published = done },
			}, strings.NewReader(answers), &output, &output,
				func(_ context.Context, args []string, _, _ io.Writer) error {
					log.add(args)
					// Publishing fails while the cluster preview runs.
					if publishing(args) && publications.Add(1) == 1 {
						if err := waitFor(planning, "the cluster preview"); err != nil {
							return err
						}
						return imagesErr
					}
					if args[0] == "plan" && !slices.Contains(args, "--registry-only") {
						if previews.Add(1) == 1 {
							close(planning)
						}
						return waitFor(published, "image publishing")
					}
					return nil
				})
			text := output.String()
			registry := strings.Index(text, "Apply these planned changes?")
			cluster := strings.LastIndex(text, "Apply these planned changes?")
			if offer := strings.Index(text, "Retry this stage"); offer < registry || cluster > registry && offer > cluster {
				t.Fatalf("the image failure was not raised before the cluster approval:\n%s", text)
			}
			commands := log.list()
			if !retry {
				if !errors.Is(err, imagesErr) || slices.Contains(commands, "provision") {
					t.Fatalf("declined retry still provisioned AKS: %v, %v", commands, err)
				}
				if strings.Count(text, "Apply these planned changes?") != 1 {
					t.Fatalf("setup asked to apply the cluster after an image failure:\n%s", text)
				}
				return
			}
			// The background images start after the registry exists and
			// race the first preview; the foreground retry runs between the
			// previews, and the cluster is provisioned after the second.
			want := []string{"plan.registry_only", "provision.registry_only", "plan", "plan", "provision", "check", "images.check", "prepare"}
			if got := foreground(commands); err != nil || len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
				t.Fatalf("foreground image retry did not continue setup: %v, %v\n%s", commands, err, text)
			}
			if !imagesBetweenPreviews(commands) {
				t.Fatalf("images did not run after the registry and again between the previews: %v", commands)
			}
			again := strings.Index(text, "Previewing the infrastructure again")
			if offer := strings.Index(text, "Retry this stage"); again < offer || again > cluster ||
				strings.LastIndex(text, "Preview full infrastructure") < again {
				t.Fatalf("the cluster approval did not follow a new preview:\n%s", text)
			}
		})
	}
}

// imagesBetweenPreviews reports whether commands publish images twice: first
// after the registry exists, then between two cluster previews that come
// before the cluster is provisioned.
func imagesBetweenPreviews(commands []string) bool {
	indices := func(id string) []int {
		var out []int
		for i, c := range commands {
			if c == id {
				out = append(out, i)
			}
		}
		return out
	}
	images, plans, provision := indices("images"), indices("plan"), slices.Index(commands, "provision")
	return len(images) == 2 && len(plans) == 2 &&
		images[0] > slices.Index(commands, "provision.registry_only") &&
		plans[0] < images[1] && images[1] < plans[1] && plans[1] < provision
}

// gatedReader returns each part once its gate has opened.
type gatedReader struct {
	parts [][]byte
	gates []func()
}

func (r *gatedReader) Read(p []byte) (int, error) {
	for len(r.parts) > 0 && len(r.parts[0]) == 0 {
		r.parts, r.gates = r.parts[1:], r.gates[1:]
	}
	if len(r.parts) == 0 {
		return 0, io.EOF
	}
	if r.gates[0] != nil {
		r.gates[0]()
		r.gates[0] = nil
	}
	n := copy(p, r.parts[0])
	r.parts[0] = r.parts[0][n:]
	return n, nil
}

// TestSetupImageFailureDuringApprovalRepreviews checks that images failing
// while the cluster approval is open are handled before the approval is used:
// a successful retry previews the cluster again for a new approval, and a
// declined one submits nothing.
func TestSetupImageFailureDuringApprovalRepreviews(t *testing.T) {
	imagesErr := errors.New("1 release image failed:\n[image a] unauthorized")
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprint("retry=", retry), func(t *testing.T) {
			path, c := overlapInstallation(t)
			var log stageLog
			var publications atomic.Int32
			published := make(chan (<-chan struct{}), 1)
			// The second approval prompt is the cluster's.
			output := newSignalWriter("Apply these planned changes?")
			output.count = 2
			// Continue, no demo, apply the registry; then apply the cluster
			// once publishing has failed, and answer the retry offer and the
			// cluster approval that follows a new preview. Only retry=true
			// pins the check after approval: when the retry is declined,
			// provision's own join would make the same offer.
			later := "yes\nno\n"
			if retry {
				later = "yes\nyes\nyes\n"
			}
			in := &gatedReader{
				parts: [][]byte{[]byte("yes\nno\nyes\n"), []byte(later)},
				gates: []func(){nil, func() {
					select {
					case done := <-published:
						if err := waitFor(done, "image publishing"); err != nil {
							t.Error(err)
						}
					case <-time.After(30 * time.Second):
						t.Error("image publishing did not start")
					}
				}},
			}
			err := setupForTest(t.Context(), setupOptions{
				home: recordHome(path), name: recordName(path), skipContextHandoff: true, discovery: subscriptionDisplayRunner(t),
				backgroundStarted: func(done <-chan struct{}) { published <- done },
			}, in, output, output,
				func(_ context.Context, args []string, _, _ io.Writer) error {
					if log.add(args) == "images" && publications.Add(1) == 1 {
						if err := waitFor(output.seen, "the cluster approval"); err != nil {
							return err
						}
						return imagesErr
					}
					return nil
				})
			text := output.String()
			commands := log.list()
			if !retry {
				if !errors.Is(err, imagesErr) || slices.Contains(commands, "provision") {
					t.Fatalf("declined retry still provisioned AKS: %v, %v\n%s", commands, err, text)
				}
				if err := lockFree(c); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err != nil || !imagesBetweenPreviews(commands) {
				t.Fatalf("the cluster was not previewed again after the retry: %v, %v\n%s", commands, err, text)
			}
			offer, again := strings.Index(text, "Retry this stage"), strings.Index(text, "Previewing the infrastructure again")
			approval := strings.LastIndex(text, "Apply these planned changes?")
			if strings.Count(text, "Apply these planned changes?") != 3 || offer < 0 || again < offer ||
				approval < again || strings.LastIndex(text, "Preview full infrastructure") < again {
				t.Fatalf("the cluster approval did not follow a new preview:\n%s", text)
			}
		})
	}
}

func TestSetupClusterFailureWithImages(t *testing.T) {
	provisionErr := errors.New("AKS quota exceeded")
	imagesErr := errors.New("1 release image failed:\n[image a] not found")
	for _, imagesFail := range []bool{false, true} {
		t.Run(fmt.Sprint("images fail=", imagesFail), func(t *testing.T) {
			path, c := overlapInstallation(t)
			var log stageLog
			var published <-chan struct{}
			provisioning := make(chan struct{})
			var output bytes.Buffer
			err := setupForTest(t.Context(), setupOptions{
				home: recordHome(path), name: recordName(path), yes: true, skipContextHandoff: true, discovery: subscriptionDisplayRunner(t),
				backgroundStarted: func(done <-chan struct{}) { published = done },
			}, strings.NewReader(""), &output, &output,
				func(_ context.Context, args []string, _, stderr io.Writer) error {
					log.add(args)
					if publishing(args) {
						if err := waitFor(provisioning, "AKS provisioning"); err != nil {
							return err
						}
						fmt.Fprint(stderr, "[image a]\nimport output\n")
						if imagesFail {
							return imagesErr
						}
						return nil
					}
					if args[0] == "provision" && !slices.Contains(args, "--registry-only") {
						close(provisioning)
						if err := waitFor(published, "image publishing"); err != nil {
							return err
						}
						return provisionErr
					}
					return nil
				})
			if !errors.Is(err, provisionErr) || !strings.Contains(err.Error(), "setup stopped at Provision AKS, storage, and identities") {
				t.Fatalf("cluster failure was not reported: %v", err)
			}
			if errors.Is(err, imagesErr) != imagesFail || strings.Contains(err.Error(), "also failed") != imagesFail {
				t.Fatalf("image result was misreported: %v", err)
			}
			if imagesFail {
				msg := err.Error()
				if strings.Index(msg, provisionErr.Error()) > strings.Index(msg, "Publish images and prepare runtime assets also failed") ||
					!strings.Contains(msg, "Retry: "+cliCommand("images", c.Installation.Name)) {
					t.Fatalf("joined failures were not ordered foreground first: %v", err)
				}
			}
			if !strings.Contains(output.String(), "[image a]\nimport output\n") {
				t.Fatalf("background output was not shown:\n%s", &output)
			}
			if commands := log.list(); slices.Contains(commands, "check") {
				t.Fatalf("setup continued after the cluster failed: %v", commands)
			}
			if err := lockFree(c); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSetupBackgroundOutputWaitsForPrompts(t *testing.T) {
	path, _ := overlapInstallation(t)
	var published <-chan struct{}
	planning := make(chan struct{})
	var output bytes.Buffer
	// Continue, no demo, apply the registry, apply the cluster.
	err := setupForTest(t.Context(), setupOptions{
		home: recordHome(path), name: recordName(path), accessible: true, skipContextHandoff: true, discovery: subscriptionDisplayRunner(t),
		backgroundStarted: func(done <-chan struct{}) { published = done },
	}, strings.NewReader("yes\nno\nyes\nyes\n"), &output, &output,
		func(_ context.Context, args []string, out, stderr io.Writer) error {
			if publishing(args) {
				if err := waitFor(planning, "the cluster preview"); err != nil {
					return err
				}
				fmt.Fprint(stderr, "[image a]\nimported a\n")
				fmt.Fprint(stderr, "[image b]\nimported b\n")
				fmt.Fprintln(out, "Saved ACR image references.")
				return nil
			}
			if args[0] == "plan" && !slices.Contains(args, "--registry-only") {
				// Publishing finishes while the cluster preview runs, before
				// its approval.
				close(planning)
				return waitFor(published, "image publishing")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("setup failed: %v\n%s", err, &output)
	}
	text := output.String()
	first := strings.Index(text, "Apply these planned changes?")
	approval := first + 1 + strings.Index(text[first+1:], "Apply these planned changes?")
	block := strings.Index(text, "[image a]\nimported a\n[image b]\nimported b\n")
	if first < 0 || approval <= first || block < approval || strings.Count(text, "[image a]") != 1 ||
		!strings.Contains(text[approval:], "(ran in the background)") ||
		!strings.Contains(text[approval:], "Saved ACR image references.") {
		t.Fatalf("background output did not follow the approval in order:\n%s", text)
	}
	if verify := strings.Index(text, "Verify published images before installing Substrate"); verify < block {
		t.Fatalf("background output followed image verification:\n%s", text)
	}
}

func TestWorkspaceKeepsBackgroundPhaseRunning(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		mark string
	}{
		{"succeeded", nil, "✓"},
		{"failed", errors.New("image import failed"), "✗"},
		{"cancelled", context.Canceled, "–"},
		{"failed beside cancellations", errors.Join(fmt.Errorf("importing release images stopped after 1 of 9: %w", context.Canceled), errors.New("image import failed")), "✗"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testWorkspace(t)
			m.welcome = false
			m.Update(setupPhaseMsg("images"))
			m.Update(setupBackgroundMsg{phase: "images"})
			m.Update(setupPhaseMsg("cluster"))
			rail := ansi.Strip(m.rail())
			if m.completed["images"] || !strings.Contains(rail, "● Images and assets") || !strings.Contains(rail, "● AKS cluster") {
				t.Fatalf("background phase did not keep running:\n%s", rail)
			}
			m.Update(setupBackgroundMsg{phase: "images", done: true, err: tc.err})
			rail = ansi.Strip(m.rail())
			if !strings.Contains(rail, tc.mark+" Images and assets") || m.completed["images"] != (tc.err == nil) ||
				!strings.Contains(rail, "● AKS cluster") {
				t.Fatalf("finished background phase shows the wrong state:\n%s", rail)
			}
			if tc.err != nil {
				// A foreground retry that succeeds completes the phase.
				m.Update(setupBackgroundMsg{phase: "images", done: true})
				if rail = ansi.Strip(m.rail()); !strings.Contains(rail, "✓ Images and assets") {
					t.Fatalf("retried phase did not complete:\n%s", rail)
				}
			}
		})
	}
}

func TestSetupBufferKeepsTheStartOfLongOutput(t *testing.T) {
	b := &setupBuffer{limit: 8}
	for _, chunk := range []string{"abc", "defgh", "ijk"} {
		if n, err := b.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("write = %d, %v", n, err)
		}
	}
	var out bytes.Buffer
	b.writeTo(&out)
	if got := out.String(); !strings.HasPrefix(got, "abcdefgh\n[3 more bytes omitted here;") {
		t.Fatalf("truncated output = %q", got)
	}
}
