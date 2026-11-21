package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/Azure/substrate-aks/internal/installer"
)

// setupBufferLimit caps each output stream a background stage keeps for the
// terminal. Its command log always has the full output.
const setupBufferLimit = 8 << 20

// setupBackground is a setup stage running alongside the foreground stages.
type setupBackground struct {
	stage  setupStage
	args   []string
	cancel context.CancelFunc
	done   chan struct{}
	// err is the stage result and interrupted reports that the stage was
	// cancelled before it returned; both are set before done is closed.
	err         error
	interrupted bool
	// stdout and stderr hold the output shown when the stage is joined;
	// they are nil when the output is not shown.
	stdout, stderr *setupBuffer
	lock           io.Closer
}

// startSetupBackground locks the installation for setup and starts the stage.
// Commands run under a context from setupFlow's stageCtx share that lock
// until the stage is closed. The stage reports to buffers, never to the
// workspace or the terminal, which belong to the foreground stages and their
// prompts.
func startSetupBackground(ctx context.Context, c installer.Config, timings *installer.Timings, s setupStage, args []string, buffered bool, execute commandRunner) (*setupBackground, error) {
	lock, err := installer.Lock(c)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	runCtx = context.WithValue(runCtx, setupSessionKey{}, (*setupSession)(nil))
	runCtx = installer.WithHeldLock(runCtx, c)
	runCtx, stop := timings.BackgroundStage(runCtx, setupStageID(s.command, s.flags), s.label)
	b := &setupBackground{stage: s, args: args, cancel: cancel, done: make(chan struct{}), lock: lock}
	stdout, stderr := io.Discard, io.Discard
	if buffered {
		b.stdout, b.stderr = &setupBuffer{limit: setupBufferLimit}, &setupBuffer{limit: setupBufferLimit}
		stdout, stderr = b.stdout, b.stderr
	}
	go func() {
		defer close(b.done)
		err := execute(runCtx, args, stdout, stderr)
		stop(err)
		b.err, b.interrupted = err, runCtx.Err() != nil
	}()
	return b, nil
}

// finished reports whether the stage has returned.
func (b *setupBackground) finished() bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

// wait returns the stage result once it has returned.
func (b *setupBackground) wait() error {
	<-b.done
	return b.err
}

// close shows the output of a returned stage, releases the installation lock
// and marks the stage finished in the workspace.
func (b *setupBackground) close(ui *setupUI, out, stderr io.Writer) {
	b.cancel()
	if b.stdout != nil {
		ui.section(b.stage.label + " (ran in the background)")
		b.stderr.writeTo(stderr)
		b.stdout.writeTo(out)
	}
	b.lock.Close()
	if ui.session != nil {
		ui.session.send(setupBackgroundMsg{phase: b.stage.phaseID(), done: true, err: b.err})
	}
}

// setupBuffer keeps the first limit bytes written to it.
type setupBuffer struct {
	mu      sync.Mutex
	limit   int
	data    []byte
	dropped int64
}

func (b *setupBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	keep := min(len(p), max(b.limit-len(b.data), 0))
	b.data = append(b.data, p[:keep]...)
	b.dropped += int64(len(p) - keep)
	return len(p), nil
}

func (b *setupBuffer) writeTo(w io.Writer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w.Write(b.data)
	if b.dropped > 0 {
		if len(b.data) > 0 && !bytes.HasSuffix(b.data, []byte("\n")) {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "[%d more bytes omitted here; the stage's command log in the installation's logs directory has the full output]\n", b.dropped)
	}
}
