//go:build unix

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestInterruptContext(t *testing.T) {
	if os.Getenv("SUBSTRATE_AKS_SIGNAL_TEST_CHILD") == "1" {
		ctx, stop := interruptContext(context.Background())
		defer stop()
		fmt.Println("ready")
		<-ctx.Done()
		fmt.Println("cleanup")
		time.Sleep(time.Hour)
		return
	}
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
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
			defer func() {
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
			expect("ready")
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			expect("cleanup")
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
