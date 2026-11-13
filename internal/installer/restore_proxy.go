package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type restoreProxyOutput struct {
	mu      sync.Mutex
	line    string
	address chan string
}

func (w *restoreProxyOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range p {
		if b != '\n' {
			if len(w.line) < 4096 {
				w.line += string(b)
			}
			continue
		}
		// The pinned kubectl-ate proxy prints its bound address before serving.
		// Source: b34fbee742906494b5eb4f1f36987176bf8770a4,
		// cmd/kubectl-ate/internal/cmd/proxy.go:90-104.
		if strings.HasPrefix(w.line, "Proxying ") {
			address, _, ok := strings.Cut(strings.TrimPrefix(w.line, "Proxying "), " -> ")
			host, port, err := net.SplitHostPort(address)
			if ok && err == nil && host == "127.0.0.1" {
				if n, err := strconv.Atoi(port); err == nil && n > 0 && n <= 65535 {
					select {
					case w.address <- address:
					default:
					}
				}
			}
		}
		w.line = ""
	}
	return len(p), nil
}

type restoreProxy struct {
	cancel  context.CancelFunc
	done    <-chan error
	address string
}

func (a App) startRestoreProxy(ctx, readyCtx context.Context, client, namespace, actor string) (*restoreProxy, error) {
	proxyCtx, cancel := context.WithCancel(ctx)
	output := &restoreProxyOutput{address: make(chan string, 1)}
	done := make(chan error, 1)
	args := []string{client, "--kubeconfig", a.kubeconfig(), "--context", a.Config.AKS.ClusterName,
		"proxy", "actor", actor, "-a", namespace, "--address", "127.0.0.1", "0:80"}
	go func() {
		_, err := a.Runner.Run(proxyCtx, Command{Args: args, StreamStderr: output, MaxOutput: 4096})
		done <- err
	}()
	proxy := &restoreProxy{cancel: cancel, done: done}
	select {
	case address := <-output.address:
		proxy.address = address
		return proxy, nil
	case err := <-done:
		cancel()
		if err == nil {
			err = errors.New("proxy exited before becoming ready")
		}
		return nil, err
	case <-readyCtx.Done():
		cancel()
		return proxy, readyCtx.Err()
	}
}

func (p *restoreProxy) stop(ctx context.Context) (joined bool, err error) {
	if p == nil {
		return true, nil
	}
	p.cancel()
	select {
	case err := <-p.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			return true, fmt.Errorf("stopping restore proxy: %w", err)
		}
		return true, nil
	case <-ctx.Done():
		return false, fmt.Errorf("joining restore proxy: %w", ctx.Err())
	}
}

func restoreProxyClient() *http.Client {
	// The request goes only to the loopback proxy, regardless of HTTP_PROXY.
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	return &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (a App) restoreActorReady(ctx context.Context, address string) error {
	client := restoreProxyClient()
	return a.restoreActorReadyWithClient(ctx, client, address)
}

func (a App) restoreActorReadyWithClient(ctx context.Context, client *http.Client, address string) error {
	const attempts = 4
	for attempt := 1; attempt <= attempts; attempt++ {
		a.progress("Checking actor readiness (attempt %d/%d)", attempt, attempts)
		attemptCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, "http://"+address+"/readyz", nil)
		if err != nil {
			cancel()
			return fmt.Errorf("readiness request: %w", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			if attemptCtx.Err() != nil {
				err = errors.Join(err, attemptCtx.Err())
			}
			cancel()
			return fmt.Errorf("readiness attempt %d transport or timeout: %w", attempt, err)
		}
		status := resp.StatusCode
		if err := attemptCtx.Err(); err != nil {
			resp.Body.Close()
			cancel()
			return fmt.Errorf("readiness attempt %d timeout or cancellation: %w", attempt, err)
		}
		if status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout {
			resp.Body.Close()
			cancel()
			if attempt == attempts {
				return fmt.Errorf("readiness exhausted %d attempts; last HTTP status %d", attempts, status)
			}
			timer := time.NewTimer(time.Second << (attempt - 1))
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("readiness backoff interrupted: %w", ctx.Err())
			case <-timer.C:
			}
			continue
		}
		if status != http.StatusOK {
			resp.Body.Close()
			cancel()
			return fmt.Errorf("readiness returned HTTP status %d", status)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4097))
		resp.Body.Close()
		if attemptCtx.Err() != nil {
			readErr = errors.Join(readErr, attemptCtx.Err())
		}
		cancel()
		if readErr != nil {
			return fmt.Errorf("readiness response read or timeout: %w", readErr)
		}
		if len(data) > 4096 {
			return errors.New("readiness response exceeds 4 KiB")
		}
		if string(data) != "ok\n" {
			return errors.New("malformed readiness response")
		}
		return nil
	}
	return errors.New("readiness attempts exhausted")
}

func restoreCounterCall(ctx context.Context, address string, want int) error {
	if err := restoreCounterRequest(ctx, address, want); err != nil {
		return fmt.Errorf("%w; counter POST was not retried", err)
	}
	return nil
}

func restoreCounterRequest(ctx context.Context, address string, want int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+address+"/", nil)
	if err != nil {
		return fmt.Errorf("counter POST request: %w", err)
	}
	client := restoreProxyClient()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("counter POST failed: %w", err)
	}
	defer resp.Body.Close()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("counter POST interrupted: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("counter POST returned HTTP status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	if err != nil {
		return fmt.Errorf("counter POST response read failed: %w", err)
	}
	if len(data) > 4096 {
		return errors.New("counter POST response exceeds 4 KiB")
	}
	// The counter response format is defined by the pinned
	// demos/counter/counter.go at b34fbee742906494b5eb4f1f36987176bf8770a4.
	body, prefixOK := strings.CutPrefix(string(data), "preserved memory count: ")
	memory, body, memoryOK := strings.Cut(body, " | preserved file counter: ")
	file, rootfs, fileOK := strings.Cut(body, " | preserved rootfs counter: ")
	rootfs, newlineOK := strings.CutSuffix(rootfs, "\n")
	observed, memoryErr := strconv.ParseUint(memory, 10, 64)
	fileValue, fileErr := strconv.ParseInt(file, 10, 64)
	rootfsValue, rootfsErr := strconv.ParseInt(rootfs, 10, 64)
	if !prefixOK || !memoryOK || !fileOK || !newlineOK || memoryErr != nil || fileErr != nil || rootfsErr != nil ||
		memory != strconv.FormatUint(observed, 10) || file != strconv.FormatInt(fileValue, 10) || rootfs != strconv.FormatInt(rootfsValue, 10) {
		return errors.New("malformed counter response")
	}
	if observed != uint64(want) {
		return fmt.Errorf("unexpected counter memory response: expected %d, observed %d", want, observed)
	}
	return nil
}
