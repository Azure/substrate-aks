package installer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	fmt.Fprintf(header, "$")
	for _, arg := range c.Args {
		fmt.Fprintf(header, " %q", arg)
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
	} else if c.ShowOutput {
		if r.Stdout != nil {
			stdoutWriters = append(stdoutWriters, r.Stdout)
		}
		if r.Stderr != nil {
			stderrWriters = append(stderrWriters, r.Stderr)
		}
	}
	cmd.Stdout = io.MultiWriter(stdoutWriters...)
	cmd.Stderr = io.MultiWriter(stderrWriters...)
	if err := cmd.Run(); err != nil {
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
		case "PROJECT_ID", "PROJECT_NUMBER", "BUCKET_NAME", "KUBECTL_CONTEXT", "NO_DEV_ENV", "VERSION", "ARCH", "OUT", "KATA_VER", "CH_VER":
			drop = true
		}
		if !drop {
			out = append(out, entry)
		}
	}
	return out
}
