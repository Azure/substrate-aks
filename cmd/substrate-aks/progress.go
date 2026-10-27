package main

import (
	"fmt"
	"io"
	"sync"
	"time"

	"charm.land/bubbles/v2/spinner"
)

type progress struct {
	mu             sync.Mutex
	out            io.Writer
	log            io.Writer
	console        *console
	stage          string
	started        time.Time
	done           chan struct{}
	stopped        chan struct{}
	active         bool
	spinnerVisible bool
	frame          int
	animation      spinner.Spinner
}

func newProgress(out, log io.Writer, stage string, allowAnimation bool) *progress {
	p := &progress{
		out: out, log: log, console: newConsole(out), stage: stage, started: time.Now(),
		done: make(chan struct{}), stopped: make(chan struct{}),
		animation: spinner.MiniDot,
	}
	go func() {
		defer close(p.stopped)
		interval := 20 * time.Second
		update := p.heartbeat
		if allowAnimation && p.console.animated {
			interval = p.animation.FPS
			update = p.spin
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-p.done:
				return
			case <-ticker.C:
				update()
			}
		}
	}()
	return p
}

func (p *progress) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearSpinnerLocked()
	return p.out.Write(data)
}

func (p *progress) step(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearSpinnerLocked()
	p.stage = fmt.Sprintf(format, args...)
	p.active = true
	p.console.logger.Infof("%s...", p.stage)
	fmt.Fprintf(p.log, "\n[step] %s\n", p.stage)
}

func (p *progress) heartbeat() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active {
		p.console.logger.Debugf("Still working: %s (%s total elapsed).", p.stage, time.Since(p.started).Round(time.Second))
	}
}

func (p *progress) spin() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.active {
		return
	}
	frames := p.animation.Frames
	frame := p.console.spinner.Render(frames[p.frame])
	p.frame = (p.frame + 1) % len(frames)
	fmt.Fprintf(p.out, "\r\x1b[2K  %s %s elapsed", frame, time.Since(p.started).Round(time.Second))
	p.spinnerVisible = true
}

func (p *progress) clearSpinnerLocked() {
	if p.spinnerVisible {
		fmt.Fprint(p.out, "\r\x1b[2K")
		p.spinnerVisible = false
	}
}

func (p *progress) pause() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active = false
	p.clearSpinnerLocked()
}

func (p *progress) heading(title string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearSpinnerLocked()
	p.console.heading.Print(title)
}

func (p *progress) info(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearSpinnerLocked()
	p.console.logger.Debugf(format, args...)
}

func (p *progress) warn(message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearSpinnerLocked()
	p.console.logger.Warn(message)
}

func (p *progress) complete(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active = false
	p.clearSpinnerLocked()
	p.console.success.Infof(format, args...)
}

func (p *progress) stop() {
	close(p.done)
	<-p.stopped
	p.pause()
}
