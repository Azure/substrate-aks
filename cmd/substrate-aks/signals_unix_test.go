//go:build linux || darwin

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/Azure/substrate-aks/internal/installer"
)

func TestInterruptContext(t *testing.T) {
	if os.Getenv("SUBSTRATE_AKS_SIGNAL_TEST_CHILD") == "1" {
		ctx, stop := interruptContext(context.Background())
		defer stop()
		_, err := (installer.ExecRunner{Stdout: os.Stdout}).Run(ctx, installer.Command{
			Args:       []string{"sh", "-c", `sleep 3600 & printf 'ready %s\n' "$$"; wait`},
			ShowOutput: true,
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("child process group did not stop through cancellation: %v", err)
		}
		fmt.Println("cleanup")
		time.Sleep(time.Hour)
		return
	}
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInterruptContext$")
			cmd.Env = append(os.Environ(), "SUBSTRATE_AKS_SIGNAL_TEST_CHILD=1")
			cmd.Stderr = os.Stderr
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			var workerGroup int
			defer func() {
				if workerGroup > 0 {
					if err := syscall.Kill(-workerGroup, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
						t.Errorf("cleaning up worker process group: %v", err)
					}
				}
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()
			lines := bufio.NewScanner(out)
			expect := func(want string) {
				t.Helper()
				if !lines.Scan() || lines.Text() != want {
					t.Fatalf("expected subprocess %q, got %q: %v", want, lines.Text(), lines.Err())
				}
			}
			if !lines.Scan() {
				t.Fatalf("worker never became ready: %v", lines.Err())
			}
			if _, err := fmt.Sscanf(lines.Text(), "ready %d", &workerGroup); err != nil || workerGroup <= 0 {
				t.Fatalf("invalid worker process group %q: %v", lines.Text(), err)
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			expect("cleanup")
			workerGroup = 0
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err == nil {
				t.Fatal("second signal did not terminate cleanup")
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if ctx.Err() != nil || !ok || !status.Signaled() || status.Signal() != sig {
				t.Fatalf("second signal did not restore default termination: %v, %v", cmd.ProcessState, ctx.Err())
			}
		})
	}
}
