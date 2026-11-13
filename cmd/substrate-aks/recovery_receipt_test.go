package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

type receiptLog struct {
	bytes.Buffer
	writeErr, syncErr error
	synced            bool
}

func (l *receiptLog) Write(p []byte) (int, error) {
	if l.writeErr != nil {
		return 0, l.writeErr
	}
	return l.Buffer.Write(p)
}

func (l *receiptLog) Sync() error {
	l.synced = true
	return l.syncErr
}

type failedReceiptWriter struct{}

func (failedReceiptWriter) Write([]byte) (int, error) { return 0, errors.New("output failed") }

type shortReceiptWriter struct{}

func (shortReceiptWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

type shortReceiptLog struct{ receiptLog }

func (*shortReceiptLog) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestRecoveryReceiptRejectsShortWrites(t *testing.T) {
	t.Run("log", func(t *testing.T) {
		log := &shortReceiptLog{}
		var output bytes.Buffer
		p := newProgress(&output, log, "check", false)
		defer p.stop()
		if err := p.recoveryReceipt(t.Context(), "recovery commands"); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("short log write accepted: %v", err)
		}
		if log.synced || output.Len() != 0 {
			t.Fatal("presentation continued after a short log write")
		}
	})
	t.Run("presentation", func(t *testing.T) {
		log := &receiptLog{}
		p := newProgress(shortReceiptWriter{}, log, "check", false)
		defer p.stop()
		if err := p.recoveryReceipt(t.Context(), "recovery commands"); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("short presentation accepted: %v", err)
		}
		if !log.synced {
			t.Fatal("log was not synced before presentation")
		}
	})
}

func TestRestoreRecoveryDocumentationExplainsStalledOutput(t *testing.T) {
	data, err := os.ReadFile("../../docs/operations.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, phrase := range []string{
		"cancellation and cleanup can also wait",
		"may not interrupt a blocked output write",
		"cleanup time limit still applies",
		"saves their recovery commands in the private command log and presents them to you",
		"automatic cleanup is not guaranteed",
		"checking their identities as described below",
	} {
		if !strings.Contains(text, phrase) {
			t.Errorf("missing recovery guidance: %q", phrase)
		}
	}
}

func TestRestoreRecoveryDocumentationExplainsProbeFailures(t *testing.T) {
	data, err := os.ReadFile("../../docs/operations.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(data)), " ")
	for _, phrase := range []string{
		"does not retry HTTP statuses or wait on `Retry-After`",
		"401 authentication response fails the check even if credentials are refreshed; check your sign-in and rerun",
		"429 or 503, retry explicitly after throttling or service recovery",
		"probe failure is not a busy skip and occurs before restore resources are created",
	} {
		if !strings.Contains(text, phrase) {
			t.Errorf("missing probe recovery guidance: %q", phrase)
		}
	}
}

func TestRecoveryReceiptRequiresLogAndPresentation(t *testing.T) {
	for _, tc := range []struct {
		name                string
		log                 *receiptLog
		output              io.Writer
		session             bool
		wantErr, wantSynced bool
	}{
		{"standalone", &receiptLog{}, &bytes.Buffer{}, false, false, true},
		{"log write failed", &receiptLog{writeErr: errors.New("write failed")}, io.Discard, false, true, false},
		{"log sync failed", &receiptLog{syncErr: errors.New("sync failed")}, io.Discard, false, true, true},
		{"output failed", &receiptLog{}, failedReceiptWriter{}, false, true, true},
		{"workspace stalled", &receiptLog{}, io.Discard, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			p := newProgress(tc.output, tc.log, "check", false)
			defer p.stop()
			if tc.session {
				p.session = &setupSession{ctx: ctx, events: make(chan any, 1)}
			}
			err := p.recoveryReceipt(ctx, "recovery commands")
			if (err != nil) != tc.wantErr || tc.log.synced != tc.wantSynced {
				t.Fatalf("error=%v synced=%v", err, tc.log.synced)
			}
			if !tc.wantErr && (!strings.Contains(tc.log.String(), "recovery commands") || !strings.Contains(tc.output.(*bytes.Buffer).String(), "recovery commands")) {
				t.Fatal("receipt missing from log or output")
			}
		})
	}
}

func TestRecoveryCommandReportsTerminalFailure(t *testing.T) {
	command := &setupRecoveryCommand{ctx: t.Context(), text: "recovery commands"}
	command.SetStdout(failedReceiptWriter{})
	if err := command.Run(); err == nil {
		t.Fatal("failed terminal write was acknowledged")
	}
}

func TestRecoveryReceiptDoesNotWriteAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	log := &receiptLog{}
	var output bytes.Buffer
	p := newProgress(&output, log, "check", false)
	defer p.stop()
	if err := p.recoveryReceipt(ctx, "recovery commands"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if log.Len() != 0 || log.synced || output.Len() != 0 {
		t.Fatal("cancelled receipt wrote diagnostics")
	}
}
