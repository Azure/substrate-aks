//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/substrate-aks/internal/installer"
	"golang.org/x/sys/unix"
)

type setupPTY struct {
	master, slave *os.File
	chunks        chan string
	text          string
	all           string
}

func TestWorkspacePTYImageBuilderKeepsChecklist(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	p := newSetupPTY(t, 110, 30)
	ctx, cancel := context.WithCancel(t.Context())
	path := testCLIConfig(t)
	discovery := subscriptionDisplayRunner(t)
	executed := make(chan struct{}, 1)
	done := make(chan error, 1)
	finished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("image-builder worker did not stop before terminal cleanup")
		}
	})
	go func() {
		defer close(finished)
		done <- setupForTest(ctx, setupOptions{
			path: path, discovery: discovery,
			local: defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
				switch strings.Join(c.Args, " ") {
				case "docker info --format {{json .ServerVersion}}", "docker buildx version", "podman info --format json":
					return nil, nil
				case "docker buildx inspect --bootstrap":
					return []byte("Status: running\n"), nil
				default:
					return nil, fmt.Errorf("unexpected builder probe: %v", c.Args)
				}
			}),
		}, p.slave, p.slave, p.slave, func(context.Context, []string, io.Writer, io.Writer) error {
			executed <- struct{}{}
			return errors.New("test must never provision")
		})
	}()
	p.until(t, "Quick start")
	p.send(t, "\r")
	p.until(t, "Choose an engine")
	if !strings.Contains(p.all, "✓ Azure deployment readiness") || !strings.Contains(p.all, "? Image builder") {
		t.Fatalf("image-builder selection hid its validation context:\n%s", p.all)
	}
	p.send(t, "\x1b[B\r")
	p.until(t, "Continue with this billable")
	p.send(t, "\x03")
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelling after builder selection returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("builder selection did not reach a cancellable approval")
	}
	select {
	case <-executed:
		t.Fatal("builder selection bypassed billing approval")
	default:
	}
}

func TestWorkspacePTYStablePreflightChecklist(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%t", fail), func(t *testing.T) {
			p := newSetupPTY(t, 110, 30)
			path := testCLIConfig(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			release := make(chan struct{})
			want := errors.New("SKU probe failed")
			base := successfulCloudRunner(path)
			cloud := defaultsRunner(func(ctx context.Context, c installer.Command) ([]byte, error) {
				if strings.Contains(strings.Join(c.Args, " "), "/providers/Microsoft.Compute/skus?") {
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					if fail {
						return nil, want
					}
				}
				return base.Run(ctx, c)
			})
			done := make(chan error, 1)
			executed := make(chan struct{}, 1)
			go func() {
				done <- setupForTest(ctx, setupOptions{path: path, discovery: subscriptionDisplayRunner(t), azure: cloud},
					p.slave, p.slave, p.slave, func(context.Context, []string, io.Writer, io.Writer) error {
						executed <- struct{}{}
						return errors.New("test must never provision")
					})
			}()
			p.until(t, "Quick start")
			p.send(t, "\r")
			p.until(t, "Loading the VM SKU catalog for ")
			for _, want := range []string{"Validate your deployment", "✓ Runtime tools", "○ Operator identity review", "○ Image builder"} {
				if !strings.Contains(p.all, want) {
					t.Fatalf("preflight checklist omitted %q:\n%s", want, p.all)
				}
			}
			p.send(t, "\x1b[13~")
			p.until(t, "ACTIVITY")
			p.send(t, "\x1b")
			p.until(t, "Validate your deployment")
			close(release)
			if fail {
				p.until(t, "SKU probe failed")
				if !strings.Contains(p.all, "✗ Azure deployment readiness") || strings.Contains(p.all, "✓ Azure deployment readiness") {
					t.Fatalf("failed preflight lost its truthful result:\n%s", p.all)
				}
			} else {
				p.until(t, "Continue with this billable")
			}
			p.send(t, "\x03")
			select {
			case err := <-done:
				expected := error(context.Canceled)
				if fail {
					expected = want
				}
				if !errors.Is(err, expected) {
					t.Fatalf("cancelling before billing returned %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("checklist did not hand off to a cancellable approval")
			}
			select {
			case <-executed:
				t.Fatal("checklist bypassed billing approval")
			default:
			}
		})
	}
}

func TestWorkspacePTYRetryAndPersistentHandoff(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	p := newSetupPTY(t, 110, 30)
	path := testCLIConfig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	attempts := 0
	go func() {
		done <- setupForTest(ctx, setupOptions{path: path, withDemo: true, discovery: subscriptionDisplayRunner(t)},
			p.slave, p.slave, p.slave, func(ctx context.Context, args []string, _, _ io.Writer) error {
				s := setupSessionFrom(ctx)
				if args[0] == "plan" {
					s.send(setupPlanMsg{"test-subscription", []installer.ResourceChange{{ChangeType: "Create", ResourceID: "/subscriptions/test/resourceGroups/disposable"}}})
				}
				if args[0] == "images" && !strings.Contains(strings.Join(args, " "), "--check") {
					attempts++
					if attempts == 1 {
						return errors.New("publication failed for PTY retry test")
					}
				}
				return nil
			})
	}()
	p.until(t, "Quick start")
	p.send(t, "\r")
	p.until(t, "Continue with this billable")
	p.send(t, "y\r")
	p.until(t, "Apply these planned changes")
	p.send(t, "y\r")
	p.until(t, "Retry this stage")
	if !strings.Contains(p.all, "publication failed for PTY retry test") {
		t.Fatalf("retry prompt hid the failed operation's explanation:\n%s", p.all)
	}
	p.send(t, "y\r")
	p.until(t, "Apply these planned changes")
	p.send(t, "y\r")
	p.until(t, "Import and select this kube-context")
	p.send(t, "y\r")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("completion did not exit")
	}
	p.until(t, "Delete all resources")
	if attempts != 2 || !strings.Contains(p.all, "kubectl ate --help") || strings.Count(p.all, "export KUBECONFIG=") != 1 {
		t.Fatalf("retry or persistent receipt failed: attempts=%d\n%s", attempts, p.all)
	}
	if exit, export := strings.LastIndex(p.all, "\x1b[?1049l"), strings.Index(p.all, "export KUBECONFIG="); exit < 0 || export <= exit {
		t.Fatal("shell handoff was printed inside the alternate screen")
	}
}

func TestWorkspacePTYRegistryChecklistPrecedesProgressAndApproval(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	p := newSetupPTY(t, 110, 30)
	path := testCLIConfig(t)
	ctx, cancel := context.WithCancel(t.Context())
	release := make(chan struct{}, 2)
	done := make(chan error, 1)
	finished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("registry checklist worker did not stop before terminal cleanup")
		}
	})
	go func() {
		defer close(finished)
		done <- setupForTest(ctx, setupOptions{path: path, withDemo: true, discovery: subscriptionDisplayRunner(t)},
			p.slave, p.slave, p.slave, func(ctx context.Context, args []string, _, _ io.Writer) error {
				if args[0] != "plan" {
					return errors.New("test must never provision")
				}
				s := setupSessionFrom(ctx)
				s.send(setupStepMsg("Checking Azure subscription access"))
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
				s.send(setupStepMsg("Preparing infrastructure templates"))
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
				return s.preview("test", []installer.ResourceChange{{ChangeType: "Create", ResourceID: "/resourceGroups/test"}})
			})
	}()
	p.until(t, "Quick start")
	p.send(t, "\r")
	p.until(t, "Continue with this billable")
	phaseStart := len(p.all)
	p.send(t, "y\r")
	p.until(t, "Checking Azure subscription access")
	for _, want := range []string{"Preview registry bootstrap", "○ Provision the private registry"} {
		if !strings.Contains(p.all, want) {
			t.Fatalf("registry work began without its preset checklist %q:\n%s", want, p.all)
		}
	}
	if strings.Contains(p.all[phaseStart:], "[work]") {
		t.Fatal("registry stage fell back to the rolling progress list")
	}
	release <- struct{}{}
	p.until(t, "Preparing infrastructure template")
	release <- struct{}{}
	p.until(t, "Apply these planned changes")
	p.send(t, "\x03")
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "Resources are retained") {
			t.Fatalf("registry approval lost cancellation or recovery guidance: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("registry approval did not stop")
	}
}

func TestWorkspacePTYCancellationStopsOwnedSubprocess(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	p := newSetupPTY(t, 100, 26)
	path := testCLIConfig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		done <- setupForTest(ctx, setupOptions{path: path, withDemo: true, discovery: subscriptionDisplayRunner(t)},
			p.slave, p.slave, p.slave, func(ctx context.Context, args []string, _, _ io.Writer) error {
				if args[0] == "plan" {
					return nil
				}
				setupSessionFrom(ctx).send(setupStepMsg("Waiting for cancellable subprocess"))
				_, err := (installer.ExecRunner{}).Run(ctx, installer.Command{Args: []string{"sh", "-c", "sleep 600 & wait"}})
				close(stopped)
				return err
			})
	}()
	p.until(t, "Quick start")
	p.send(t, "\r")
	p.until(t, "Continue with this billable")
	p.send(t, "y\r")
	p.until(t, "Apply these planned changes")
	p.send(t, "y\r")
	p.until(t, "Waiting for cancellable subprocess")
	p.send(t, "\x03")
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "Resources are retained") {
			t.Fatalf("cancellation lost retained-resource guidance: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("owned subprocess did not stop")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("terminal exited before owned subprocess returned")
	}
}

func newSetupPTY(t *testing.T, width, height int) *setupPTY {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	p := &setupPTY{master: master, slave: slave, chunks: make(chan string, 1024)}
	p.resize(t, width, height)
	go func() {
		defer close(p.chunks)
		buffer := make([]byte, 32768)
		for {
			n, err := master.Read(buffer)
			if n > 0 {
				p.chunks <- string(buffer[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return p
}

func (p *setupPTY) resize(t *testing.T, width, height int) {
	t.Helper()
	if err := unix.IoctlSetWinsize(int(p.slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: uint16(width), Row: uint16(height)}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Kill(os.Getpid(), unix.SIGWINCH); err != nil {
		t.Fatal(err)
	}
}

func (p *setupPTY) send(t *testing.T, text string) {
	t.Helper()
	if _, err := io.WriteString(p.master, text); err != nil {
		t.Fatal(err)
	}
}

func (p *setupPTY) until(t *testing.T, text string) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for !strings.Contains(p.text, text) {
		select {
		case chunk, ok := <-p.chunks:
			if !ok {
				t.Fatalf("PTY closed waiting for %q:\n%s", text, p.text)
			}
			p.text += chunk
			p.all += chunk
			// Answer terminal capability queries without requiring an emulator.
			if strings.Contains(chunk, "\x1b[c") {
				p.send(t, "\x1b[?1;2c")
			}
			if strings.Contains(chunk, "\x1b[6n") {
				p.send(t, "\x1b[1;1R")
			}
		case <-deadline.C:
			t.Fatalf("PTY timed out waiting for %q:\n%s", text, p.text)
		}
	}
	p.text = ""
}

func TestWorkspacePTYInputResizeAndPromptCancellation(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	for _, size := range [][2]int{{110, 30}, {48, 18}, {80, 14}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			p := newSetupPTY(t, size[0], size[1])
			before, err := unix.IoctlGetTermios(int(p.slave.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			path := testCLIConfig(t)
			done := make(chan error, 1)
			go func() {
				done <- setupForTest(ctx, setupOptions{path: path, discovery: subscriptionDisplayRunner(t)},
					p.slave, p.slave, p.slave, func(context.Context, []string, io.Writer, io.Writer) error {
						return errors.New("no provisioning authorized by this test")
					})
			}()
			p.until(t, "Quick start")
			p.send(t, "\r")
			p.until(t, "Continue with this billable")
			// Escape-prefixed details keys must not submit or consume the form.
			p.send(t, "\x1bOQ")
			p.until(t, "DETAILS")
			p.resize(t, 72, 20)
			p.send(t, "\x1b")
			p.until(t, "Continue with this billable")
			if size[0] == 48 {
				cancel()
			} else {
				p.send(t, "\x03")
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("prompt cancellation lost: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("prompt cancellation did not exit")
			}
			after, err := unix.IoctlGetTermios(int(p.slave.Fd()), unix.TCGETS)
			if err != nil || *before != *after {
				t.Fatalf("terminal was not restored: %v", err)
			}
		})
	}
}

func TestWorkspacePTYConfigurationInputAndRegionFilter(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	p := newSetupPTY(t, 100, 28)
	path := filepath.Join(t.TempDir(), "deploy.json")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- setupForTest(ctx, setupOptions{
			path: path, subscription: defaultSubscription, publisher: defaultPublisher,
			operatorID: defaultPublisher, cidr: "203.0.113.8/32", location: "eastus2",
			discovery: subscriptionDisplayRunner(t),
		}, p.slave, p.slave, p.slave, func(context.Context, []string, io.Writer, io.Writer) error {
			return errors.New("configuration test must never provision")
		})
	}()
	p.until(t, "Quick start")
	p.send(t, "\r")
	p.until(t, "Dedicated resource group")
	p.send(t, "guided-pty-test\r")
	p.until(t, "Azure region")
	p.send(t, "/southcentral")
	p.send(t, "\r")
	p.until(t, "Grant this Azure identity")
	p.send(t, "n\r")
	p.until(t, "Continue with this billable")
	p.send(t, "\x03")
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("configuration input cancellation hung")
	}
	c, err := installer.LoadConfig(path)
	if err != nil || c.Azure.ResourceGroup != "guided-pty-test" || c.Azure.Location != "southcentralus" {
		t.Fatalf("visible input did not match saved answers: %s %s %v", c.Azure.ResourceGroup, c.Azure.Location, err)
	}
}

func TestWorkspacePTYDestructiveApprovalRequiresAcknowledgement(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	p := newSetupPTY(t, 100, 26)
	path := testCLIConfig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	provisioned := make(chan struct{}, 1)
	go func() {
		done <- setupForTest(ctx, setupOptions{path: path, withDemo: true, discovery: subscriptionDisplayRunner(t)},
			p.slave, p.slave, p.slave, func(ctx context.Context, args []string, _, _ io.Writer) error {
				if args[0] == "plan" {
					var changes []installer.ResourceChange
					for i := range 12 {
						changes = append(changes, installer.ResourceChange{ChangeType: "Delete",
							ResourceID: fmt.Sprintf("/subscriptions/test/resourceGroups/disposable/providers/Microsoft.Storage/storageAccounts/account-%d", i)})
					}
					return setupSessionFrom(ctx).preview("test", changes)
				}
				provisioned <- struct{}{}
				return errors.New("unapproved provisioning")
			})
	}()
	p.until(t, "Quick start")
	p.send(t, "\r")
	p.until(t, "Continue with this billable")
	p.send(t, "y\r")
	p.until(t, "F4 acknowledge summary")
	p.send(t, "y\r\x1bOR")
	p.until(t, "ACTIVITY")
	select {
	case <-provisioned:
		t.Fatal("hidden approval accepted before destructive review")
	default:
	}
	p.send(t, "\x1b")
	p.until(t, "F4 acknowledge summary")
	p.send(t, strings.Repeat("\x1b[6~", 20)+"\x1bOS")
	p.until(t, "Apply these planned changes")
	p.send(t, "n\r")
	p.until(t, "Installation stopped")
	p.send(t, "\x03")
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no deployment was submitted") {
			t.Fatalf("declined plan lost its result: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("failure-screen exit hung")
	}
	select {
	case <-provisioned:
		t.Fatal("declining the plan still provisioned")
	default:
	}
}
