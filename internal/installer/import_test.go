package installer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const importLoginServer = "substratetest123.azurecr.io"

var importRegistry = Registry{Name: "substratetest123", LoginServer: importLoginServer}

// importFake answers acr import and repository show calls concurrently and
// records how many imports ran at once.
type importFake struct {
	mu          sync.Mutex
	inFlight    int
	maxInFlight int
	imported    []string
	shown       []string
	// started receives each image as its import starts, if set.
	started chan string
	// gate holds imports until it is closed or the context ends, if set.
	gate      chan struct{}
	importErr map[string]error
	showErr   map[string]error
	digest    map[string]string
}

func (f *importFake) Run(ctx context.Context, c Command) ([]byte, error) {
	args := strings.Join(c.Args, " ")
	var name string
	for i := 0; i+1 < len(c.Args); i++ {
		if c.Args[i] == "--image" {
			name = strings.TrimPrefix(strings.Split(c.Args[i+1], ":")[0], "substrate/")
		}
	}
	switch {
	case strings.HasPrefix(args, "az acr import "):
		f.mu.Lock()
		f.inFlight++
		f.maxInFlight = max(f.maxInFlight, f.inFlight)
		f.imported = append(f.imported, name)
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			f.inFlight--
			f.mu.Unlock()
		}()
		if f.started != nil {
			f.started <- name
		}
		if f.gate != nil {
			select {
			case <-f.gate:
			case <-ctx.Done():
				return nil, fmt.Errorf("az interrupted: %w", ctx.Err())
			}
		}
		return nil, f.importErr[name]
	case strings.HasPrefix(args, "az acr repository show "):
		f.mu.Lock()
		f.shown = append(f.shown, name)
		f.mu.Unlock()
		if err := f.showErr[name]; err != nil {
			return nil, err
		}
		digest := f.digest[name]
		if digest == "" {
			digest = testImageDigest(name)
		}
		return []byte(`{"digest":"` + digest + `"}`), nil
	}
	return nil, fmt.Errorf("unexpected command: %s", args)
}

func testImageDigest(name string) string {
	return fmt.Sprintf("sha256:%064x", len(name)*7919+int(name[len(name)-1]))
}

func testImages(n int) []releaseImage {
	images := make([]releaseImage, n)
	for i := range images {
		name := fmt.Sprintf("image-%02d", i)
		images[i] = releaseImage{name, testImageDigest(name)}
	}
	return images
}

func testImportApp(runner Runner, progress *[]string) App {
	c := DefaultConfig()
	c.Azure.SubscriptionID = "22222222-2222-4222-8222-222222222222"
	return App{Config: c, Runner: runner, Progress: func(format string, args ...any) {
		*progress = append(*progress, fmt.Sprintf(format, args...))
	}}
}

func TestReleaseImportsRunInABoundedPool(t *testing.T) {
	inventory := testImages(11)
	fake := &importFake{started: make(chan string, len(inventory)), gate: make(chan struct{})}
	var progress []string
	timings := NewTimings(time.Now)
	timings.Phase("images", "Images and assets")
	ctx := WithTimings(t.Context(), timings)
	done := make(chan error, 1)
	go func() {
		done <- testImportApp(fake, &progress).importImages(ctx, importRegistry, "substrate", "v1.0.0", inventory)
	}()
	for range importWorkers {
		<-fake.started
	}
	select {
	case name := <-fake.started:
		t.Fatalf("%s started while %d imports were running", name, importWorkers)
	case <-time.After(50 * time.Millisecond):
	}
	close(fake.gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if fake.maxInFlight != importWorkers || len(fake.imported) != len(inventory) || len(fake.shown) != len(inventory) {
		t.Fatalf("pool ran %d at once and imported %d, verified %d", fake.maxInFlight, len(fake.imported), len(fake.shown))
	}
	if progress[0] != "Importing 11 release images into "+importLoginServer+", up to 4 at a time" {
		t.Fatalf("unexpected first progress line: %q", progress[0])
	}
	for _, image := range inventory {
		if !slices.ContainsFunc(progress, func(line string) bool {
			return strings.HasPrefix(line, "Imported and verified "+image.Name+" (")
		}) {
			t.Fatalf("progress did not name %s: %v", image.Name, progress)
		}
	}
	if !strings.HasSuffix(progress[len(progress)-1], "(11/11)") {
		t.Fatalf("progress did not count completions: %v", progress)
	}
	steps := timings.Report(TimingSucceeded).Steps()
	if len(steps) != 2*len(inventory)+1 || !slices.ContainsFunc(steps, func(s StepTimingReport) bool {
		return s.ID == "images.imports" && s.Workers == importWorkers && s.Outcome == TimingSucceeded
	}) {
		t.Fatalf("imports were not timed per image with the worker count: %+v", steps)
	}
}

func TestReleaseImportWorkersAreCapped(t *testing.T) {
	for _, tc := range []struct{ override, images, want int }{
		{0, 1, 1}, {1, 11, 1}, {100, 11, maxImportWorkers}, {-1, 2, 2},
	} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			fake := &importFake{}
			var progress []string
			app := testImportApp(fake, &progress)
			app.importWorkers = tc.override
			if err := app.importImages(t.Context(), importRegistry, "substrate", "v1.0.0", testImages(tc.images)); err != nil {
				t.Fatal(err)
			}
			if want := fmt.Sprintf("up to %d at a time", tc.want); !strings.HasSuffix(progress[0], want) || fake.maxInFlight > tc.want {
				t.Fatalf("want %s, got %q with %d in flight", want, progress[0], fake.maxInFlight)
			}
		})
	}
}

func TestReleaseImportsReportEveryFailureInOrder(t *testing.T) {
	inventory := testImages(5)
	denied := errors.New("authorization failed")
	throttled := errors.New("too many requests")
	unavailable := errors.New("registry unavailable")
	fake := &importFake{
		importErr: map[string]error{"image-01": denied, "image-04": throttled},
		digest:    map[string]string{"image-02": "sha256:" + strings.Repeat("f", 64)},
		showErr:   map[string]error{"image-03": unavailable},
	}
	var progress []string
	err := testImportApp(fake, &progress).importImages(t.Context(), importRegistry, "substrate", "v1.0.0", inventory)
	for _, want := range []error{denied, throttled, unavailable} {
		if !errors.Is(err, want) {
			t.Fatalf("lost %v: %v", want, err)
		}
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "4 of 5 release images were not imported and verified:\n") {
		t.Fatalf("failures were not summarized: %v", err)
	}
	order := []string{"importing image-01", "imported substrate/image-02:v1.0.0 does not have the verified release digest", "verifying imported image-03: registry unavailable", "importing image-04"}
	last := -1
	for _, part := range order {
		i := strings.Index(msg, part)
		if i <= last {
			t.Fatalf("%q is missing or out of inventory order:\n%s", part, msg)
		}
		last = i
	}
	if strings.Contains(msg, "image-00") {
		t.Fatalf("a verified image was reported as failed:\n%s", msg)
	}
	slices.Sort(fake.imported)
	slices.Sort(fake.shown)
	if len(fake.imported) != 5 || !slices.Equal(fake.shown, []string{"image-00", "image-02", "image-03"}) {
		t.Fatalf("not every image was attempted, or a failed import was verified: %v, %v", fake.imported, fake.shown)
	}
	if !slices.ContainsFunc(progress, func(line string) bool {
		return strings.HasPrefix(line, "Could not import and verify image-04 (")
	}) {
		t.Fatalf("progress did not name the failure: %v", progress)
	}
}

func TestReleaseImportSingleFailureIsUnchanged(t *testing.T) {
	denied := errors.New("authorization failed")
	inventory := testImages(3)
	fake := &importFake{importErr: map[string]error{"image-01": denied}}
	var progress []string
	err := testImportApp(fake, &progress).importImages(t.Context(), importRegistry, "substrate", "v1.0.0", inventory)
	if !errors.Is(err, denied) || !strings.HasPrefix(err.Error(), "importing image-01 (the upload identity needs") {
		t.Fatalf("a single failure was rewrapped: %v", err)
	}
}

func TestReleaseImportsRejectInventoryBeforeImporting(t *testing.T) {
	duplicate := append(testImages(3), testImages(2)[1])
	badLate := testImages(4)
	badLate[3].Digest = "sha256:not-a-digest"
	badName := testImages(3)
	badName[2].Name = "../escape"
	for name, tc := range map[string]struct {
		inventory []releaseImage
		want      string
	}{
		"duplicate":  {duplicate, "lists image-01 more than once; nothing was imported"},
		"bad digest": {badLate, "invalid image reference"},
		"bad name":   {badName, "invalid image reference"},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &importFake{}
			var progress []string
			err := testImportApp(fake, &progress).importImages(t.Context(), importRegistry, "substrate", "v1.0.0", tc.inventory)
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(fake.imported) != 0 || len(progress) != 0 {
				t.Fatalf("inventory was not rejected before importing: %v, imported %v", err, fake.imported)
			}
		})
	}
}

func TestReleaseImportCancellationStopsScheduling(t *testing.T) {
	inventory := testImages(8)
	fake := &importFake{started: make(chan string, len(inventory)), gate: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var progress []string
	app := testImportApp(fake, &progress)
	app.importWorkers = 2
	done := make(chan error, 1)
	go func() { done <- app.importImages(ctx, importRegistry, "substrate", "v1.0.0", inventory) }()
	<-fake.started
	<-fake.started
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "importing release images stopped after 2 of 8") {
		t.Fatalf("cancellation was not reported: %v", err)
	}
	if len(fake.imported) != 2 || len(fake.shown) != 0 {
		t.Fatalf("cancelled imports kept scheduling: %v", fake.imported)
	}
}

// TestReleaseImportOutputStaysInInventoryOrder runs imports through
// ExecRunner with a fake az that finishes in reverse order, and checks the
// log and verbose output are grouped per image in inventory order.
func TestReleaseImportOutputStaysInInventoryOrder(t *testing.T) {
	inventory := testReleaseInventory
	dir := t.TempDir()
	var script strings.Builder
	script.WriteString("#!/bin/sh\nPATH=/bin:/usr/bin\ncase \"$*\" in\n")
	for i, image := range inventory {
		delay := fmt.Sprintf("0.%d", 3*(len(inventory)-i))
		fmt.Fprintf(&script, "*\"acr import\"*\"/%s@\"*) sleep %s; echo \"import output for %s\" ;;\n", image.Name, delay, image.Name)
		fmt.Fprintf(&script, "*\"repository show\"*\"substrate/%s:\"*) echo '{\"digest\":\"%s\"}' ;;\n", image.Name, image.Digest)
	}
	script.WriteString("*) echo \"unexpected: $*\" >&2; exit 1 ;;\nesac\n")
	if err := os.WriteFile(filepath.Join(dir, "az"), []byte(script.String()), 0700); err != nil {
		t.Fatal(err)
	}
	// Only the fake az is reachable.
	t.Setenv("PATH", dir)
	var log, verbose bytes.Buffer
	var progress []string
	app := testImportApp(ExecRunner{Log: &log, Verbose: &verbose}, &progress)
	if err := app.importImages(t.Context(), importRegistry, "substrate", "v1.0.0", inventory); err != nil {
		t.Fatalf("%v\n%s", err, &log)
	}
	if want := "Imported and verified " + inventory[len(inventory)-1].Name + " (1/3)"; progress[1] != want {
		t.Fatalf("the fake az did not finish in reverse order: %v", progress)
	}
	for name, out := range map[string]string{"log": log.String(), "verbose": verbose.String()} {
		last := -1
		for i, image := range inventory {
			start := strings.Index(out, "[image "+image.Name+"]\n")
			if start <= last {
				t.Fatalf("%s block for %s is missing or out of order:\n%s", name, image.Name, out)
			}
			block := out[start:]
			if i+1 < len(inventory) {
				end := strings.Index(block, "[image "+inventory[i+1].Name+"]")
				if end < 0 {
					t.Fatalf("%s block for %s is not followed by %s:\n%s", name, image.Name, inventory[i+1].Name, out)
				}
				block = block[:end]
			}
			if strings.Count(block, `"acr" "import"`) != 1 || strings.Count(block, `"repository" "show"`) != 1 {
				t.Fatalf("%s block for %s does not hold exactly its commands:\n%s", name, image.Name, block)
			}
			for _, other := range inventory {
				if other.Name != image.Name && strings.Contains(block, "/"+other.Name+"@") {
					t.Fatalf("%s block for %s holds %s output:\n%s", name, image.Name, other.Name, block)
				}
			}
			last = start
		}
	}
	if !strings.Contains(log.String(), "import output for ateapi") {
		t.Fatalf("command output was not logged:\n%s", &log)
	}
}
