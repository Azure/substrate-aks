package main

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

func interruptContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	stop := sync.OnceFunc(func() {
		// Restore termination before publishing cancellation to cleanup code.
		signal.Stop(signals)
		cancel()
	})
	go func() {
		select {
		case <-signals:
		case <-ctx.Done():
		}
		stop()
	}()
	return ctx, stop
}
