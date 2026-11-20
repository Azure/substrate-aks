package installer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
)

type Command struct {
	Dir             string
	Args            []string
	Env             []string
	Input           []byte
	ShowOutput      bool
	SensitiveOutput bool
	// BinaryOutput keeps non-text stdout out of the logs; stderr is still
	// logged for diagnostics.
	BinaryOutput bool
	// Redact lists secret values replaced in logged and displayed output,
	// the command echo and failure excerpts. Captured stdout is returned as is.
	Redact []string
}

type Runner interface {
	Run(context.Context, Command) ([]byte, error)
}

type ExecRunner struct {
	Log     io.Writer
	Verbose io.Writer
	Stdout  io.Writer
	Stderr  io.Writer
}

type lockedWriter struct {
	mu  sync.Mutex
	dst io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dst.Write(p)
}

func (r ExecRunner) Run(ctx context.Context, c Command) ([]byte, error) {
	if len(c.Args) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	log := r.Log
	if log == nil {
		log = io.Discard
	}
	verbose := r.Verbose
	if verbose == nil {
		verbose = io.Discard
	}
	header := io.MultiWriter(log, verbose)
	redact := newRedactor(c.Redact)
	fmt.Fprintf(header, "$")
	for _, arg := range c.Args {
		fmt.Fprintf(header, " %q", redact.replace(arg))
	}
	fmt.Fprintln(header)
	cmd := exec.CommandContext(ctx, c.Args[0], c.Args[1:]...)
	configureProcess(cmd)
	cmd.Dir = c.Dir
	cmd.Env = append(cleanEnvironment(os.Environ()), c.Env...)
	cmd.Stdin = bytes.NewReader(c.Input)
	var stdout bytes.Buffer
	var stderr tailBuffer
	diagnostics := log
	if !c.ShowOutput {
		diagnostics = io.MultiWriter(log, verbose)
	}
	output := &lockedWriter{dst: diagnostics}
	stdoutWriters := []io.Writer{&stdout, output}
	stderrWriters := []io.Writer{&stderr, output}
	if c.SensitiveOutput {
		stdoutWriters = []io.Writer{&stdout}
		stderrWriters = []io.Writer{io.Discard}
	} else if c.BinaryOutput {
		stdoutWriters = []io.Writer{&stdout}
		fmt.Fprintln(log, "[binary output not logged]")
	} else if c.ShowOutput {
		if r.Stdout != nil {
			stdoutWriters = append(stdoutWriters, r.Stdout)
		}
		if r.Stderr != nil {
			stderrWriters = append(stderrWriters, r.Stderr)
		}
	}
	var flush []*redactingWriter
	if redact != nil {
		// Only the captured stdout keeps the original text.
		redactedStdout := &redactingWriter{redactor: redact, dst: io.MultiWriter(stdoutWriters[1:]...)}
		redactedStderr := &redactingWriter{redactor: redact, dst: io.MultiWriter(stderrWriters...)}
		stdoutWriters = []io.Writer{&stdout, redactedStdout}
		stderrWriters = []io.Writer{redactedStderr}
		flush = append(flush, redactedStdout, redactedStderr)
	}
	cmd.Stdout = io.MultiWriter(stdoutWriters...)
	cmd.Stderr = io.MultiWriter(stderrWriters...)
	err := cmd.Run()
	for _, w := range flush {
		w.Flush()
	}
	if err != nil {
		fmt.Fprintf(header, "\n[failed] %v\n", err)
		if ctx.Err() != nil {
			return stdout.Bytes(), fmt.Errorf("%s interrupted: %w", c.Args[0], ctx.Err())
		}
		detail := strings.TrimSpace(string(stderr.data))
		lines := strings.Split(detail, "\n")
		if len(lines) > 3 {
			detail = strings.Join(lines[len(lines)-3:], "\n")
		}
		if detail != "" {
			return stdout.Bytes(), fmt.Errorf("%s failed: %w\n%s", c.Args[0], err, detail)
		}
		return stdout.Bytes(), fmt.Errorf("%s failed: %w", c.Args[0], err)
	}
	fmt.Fprintln(log)
	return stdout.Bytes(), nil
}

type redactor struct {
	values  []string
	longest int
}

// newRedactor returns nil when there is nothing to redact.
func newRedactor(values []string) *redactor {
	r := &redactor{}
	for _, value := range values {
		if value != "" && !slices.Contains(r.values, value) {
			r.values = append(r.values, value)
			r.longest = max(r.longest, len(value))
		}
	}
	if len(r.values) == 0 {
		return nil
	}
	// Replace longer values first, so a DSN is redacted before its password.
	slices.SortFunc(r.values, func(a, b string) int { return len(b) - len(a) })
	return r
}

func (r *redactor) replace(text string) string {
	if r == nil {
		return text
	}
	for _, value := range r.values {
		text = strings.ReplaceAll(text, value, "[redacted]")
	}
	return text
}

// redactingWriter redacts whole lines, so a value split across writes is still
// replaced. Overlong lines are flushed early, holding back enough bytes to
// complete any value that continues in the next write.
type redactingWriter struct {
	redactor *redactor
	dst      io.Writer
	pending  []byte
}

const redactLineLimit = 64 << 10

func (w *redactingWriter) Write(p []byte) (int, error) {
	w.pending = append(w.pending, p...)
	if i := bytes.LastIndexByte(w.pending, '\n'); i >= 0 {
		if err := w.emit(w.pending[:i+1]); err != nil {
			return 0, err
		}
		w.pending = w.pending[i+1:]
	}
	if len(w.pending) > redactLineLimit {
		text := []byte(w.redactor.replace(string(w.pending)))
		keep := min(len(text), w.redactor.longest-1)
		if _, err := w.dst.Write(text[:len(text)-keep]); err != nil {
			return 0, err
		}
		w.pending = append([]byte(nil), text[len(text)-keep:]...)
	}
	return len(p), nil
}

func (w *redactingWriter) emit(p []byte) error {
	_, err := io.WriteString(w.dst, w.redactor.replace(string(p)))
	return err
}

// Flush writes any final partial line.
func (w *redactingWriter) Flush() {
	if len(w.pending) != 0 {
		_ = w.emit(w.pending)
		w.pending = nil
	}
}

type tailBuffer struct {
	data []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	const limit = 4096
	n := len(p)
	if len(p) >= limit {
		b.data = append(b.data[:0], p[len(p)-limit:]...)
	} else {
		if excess := len(b.data) + len(p) - limit; excess > 0 {
			b.data = b.data[excess:]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}

func cleanEnvironment(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		drop := false
		for _, prefix := range []string{"ATE_", "KO_", "AWS_", "GOOGLE_", "GCLOUD_", "CLOUDSDK_", "CSQL_", "SNAPSHOT_", "GCP_", "GCE_", "CLUSTER_", "OTEL_", "ATENET_"} {
			drop = drop || strings.HasPrefix(key, prefix)
		}
		switch key {
		case "PROJECT_ID", "PROJECT_NUMBER", "BUCKET_NAME", "KUBECTL_CONTEXT", "NO_DEV_ENV", "VERSION", "ARCH", "OUT", "KATA_VER", "CH_VER",
			"ACTOR_JWT_ALGORITHM", "ACTOR_JWT_ISSUER", "EXPECTED_JWT_ISSUER", "DOCKER_BUILD_FLAGS",
			"E2E_ATE_ENVOY_CONCURRENCY", "KIND_CLUSTER_NAME", "BENCHMARK_ACTOR_MEMORY", "ANTHROPIC_API_KEY",
			"SETUP_CSI", "STORAGE_CLASS":
			drop = true
		}
		if !drop {
			out = append(out, entry)
		}
	}
	return out
}
