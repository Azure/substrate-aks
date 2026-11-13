package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type restoreHTTPRoundTrip func(*http.Request) (*http.Response, error)

func (f restoreHTTPRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type restoreReadinessBody struct {
	reader io.Reader
	reads  int
	closed bool
}

func (b *restoreReadinessBody) Read(p []byte) (int, error) {
	b.reads++
	return b.reader.Read(p)
}

func (b *restoreReadinessBody) Close() error {
	b.closed = true
	return nil
}

type restoreErrorReader struct{ err error }

func (r restoreErrorReader) Read([]byte) (int, error) { return 0, r.err }

type restoreContextReader struct{ ctx context.Context }

func (r restoreContextReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func TestRestoreReadinessRetriesOnlyGatewayStatuses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []int
		wantErr  string
	}{
		{"gateway timeout", []int{504, 200}, ""},
		{"gateway and unavailable", []int{502, 503, 200}, ""},
		{"unavailable", []int{503, 200}, ""},
		{"exhausted", []int{502, 503, 504, 503}, "exhausted 4 attempts; last HTTP status 503"},
		{"unauthorized", []int{401}, "HTTP status 401"},
		{"throttled", []int{429}, "HTTP status 429"},
		{"server error", []int{500}, "HTTP status 500"},
		{"redirect", []int{302}, "HTTP status 302"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				var times []time.Duration
				var bodies []*restoreReadinessBody
				client := &http.Client{Transport: restoreHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
					if r.Method != http.MethodGet || r.URL.Path != "/readyz" {
						t.Errorf("unexpected readiness request: %s %s", r.Method, r.URL.Path)
					}
					if r.Body != nil && r.Body != http.NoBody {
						t.Error("readiness request has a body")
					}
					deadline, ok := r.Context().Deadline()
					if !ok || deadline.Sub(time.Now()) != 15*time.Second {
						t.Errorf("attempt deadline=%v", deadline)
					}
					times = append(times, time.Since(start))
					index := len(times) - 1
					if index >= len(tc.statuses) {
						t.Fatal("unexpected extra request")
					}
					body := &restoreReadinessBody{reader: strings.NewReader("ok\n")}
					bodies = append(bodies, body)
					return &http.Response{StatusCode: tc.statuses[index], Body: body, Header: http.Header{"Retry-After": {"99"}}}, nil
				})}
				err := (App{}).restoreActorReadyWithClient(t.Context(), client, "127.0.0.1:1")
				if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
					t.Fatalf("unexpected result: %v", err)
				}
				if len(times) != len(tc.statuses) {
					t.Fatalf("attempts=%d", len(times))
				}
				wantTimes := []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second}
				for i, body := range bodies {
					if times[i] != wantTimes[i] || !body.closed || (tc.statuses[i] != 200 && body.reads != 0) {
						t.Fatalf("attempt %d time=%v reads=%d closed=%v", i+1, times[i], body.reads, body.closed)
					}
				}
			})
		})
	}
}

func TestRestoreReadinessFailuresAndDeadlines(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		reader     io.Reader
		phase      time.Duration
		cancel     bool
		stall      bool
		want       error
		reason     string
		elapsed    time.Duration
	}{
		{"malformed", "bad", nil, 0, false, false, nil, "malformed readiness", 0},
		{"at body limit", strings.Repeat("x", 4096), nil, 0, false, false, nil, "malformed readiness", 0},
		{"oversized", strings.Repeat("x", 4097), nil, 0, false, false, nil, "exceeds 4 KiB", 0},
		{"truncated", "", restoreErrorReader{io.ErrUnexpectedEOF}, 0, false, false, io.ErrUnexpectedEOF, "response read", 0},
		{"attempt timeout", "", nil, 0, false, true, context.DeadlineExceeded, "timeout", 15 * time.Second},
		{"phase timeout", "", nil, 3 * time.Second, false, true, context.DeadlineExceeded, "timeout", 3 * time.Second},
		{"cancel attempt", "", nil, 0, true, true, context.Canceled, "transport or timeout", time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				var stop context.CancelFunc
				if tc.phase != 0 {
					ctx, stop = context.WithTimeout(ctx, tc.phase)
					defer stop()
				} else if tc.cancel {
					ctx, stop = context.WithCancel(ctx)
					defer stop()
					go func() { time.Sleep(time.Second); stop() }()
				}
				start := time.Now()
				var body *restoreReadinessBody
				calls := 0
				client := &http.Client{Transport: restoreHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
					calls++
					if tc.phase != 0 {
						deadline, ok := r.Context().Deadline()
						if !ok || deadline.Sub(time.Now()) != tc.phase {
							t.Errorf("attempt deadline was not clipped: %v", deadline)
						}
					}
					if tc.stall {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					reader := tc.reader
					if reader == nil {
						reader = strings.NewReader(tc.body)
					}
					body = &restoreReadinessBody{reader: reader}
					return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
				})}
				err := (App{}).restoreActorReadyWithClient(ctx, client, "127.0.0.1:1")
				if err == nil || !strings.Contains(err.Error(), tc.reason) || tc.want != nil && !errors.Is(err, tc.want) ||
					time.Since(start) != tc.elapsed || calls != 1 || body != nil && !body.closed {
					t.Fatalf("err=%v elapsed=%v calls=%d body=%v", err, time.Since(start), calls, body)
				}
			})
		})
	}
}

func TestRestoreReadinessCancellationDuringBackoff(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint("deadline=", deadline), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var ctx context.Context
				var cancel context.CancelFunc
				want := context.Canceled
				if deadline {
					ctx, cancel = context.WithTimeout(t.Context(), 500*time.Millisecond)
					want = context.DeadlineExceeded
				} else {
					ctx, cancel = context.WithCancel(t.Context())
					go func() { time.Sleep(500 * time.Millisecond); cancel() }()
				}
				defer cancel()
				calls := 0
				client := &http.Client{Transport: restoreHTTPRoundTrip(func(*http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("ignored")), Header: make(http.Header)}, nil
				})}
				err := (App{}).restoreActorReadyWithClient(ctx, client, "127.0.0.1:1")
				if !errors.Is(err, want) || calls != 1 || !strings.Contains(err.Error(), "backoff") {
					t.Fatalf("err=%v calls=%d", err, calls)
				}
			})
		})
	}
}

func TestRestoreReadinessBodyDeadlineAndTransportFailure(t *testing.T) {
	t.Run("body deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			var body *restoreReadinessBody
			client := &http.Client{Transport: restoreHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
				body = &restoreReadinessBody{reader: restoreContextReader{ctx: r.Context()}}
				return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
			})}
			err := (App{}).restoreActorReadyWithClient(t.Context(), client, "127.0.0.1:1")
			if !errors.Is(err, context.DeadlineExceeded) || !body.closed || time.Since(start) != 15*time.Second {
				t.Fatalf("error=%v elapsed=%v closed=%v", err, time.Since(start), body.closed)
			}
		})
	})
	t.Run("transport", func(t *testing.T) {
		want := errors.New("connection failed")
		calls := 0
		client := &http.Client{Transport: restoreHTTPRoundTrip(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, want
		})}
		err := (App{}).restoreActorReadyWithClient(t.Context(), client, "127.0.0.1:1")
		if !errors.Is(err, want) || calls != 1 {
			t.Fatalf("error=%v calls=%d", err, calls)
		}
	})
}

func TestRestoreCounterRejectsRedirect(t *testing.T) {
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Store(true) }))
	defer target.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer proxy.Close()
	if err := restoreCounterCall(t.Context(), strings.TrimPrefix(proxy.URL, "http://"), 1); err == nil || followed.Load() {
		t.Fatal("counter redirect was followed or accepted")
	}
}

func TestRestoreReadinessRejectsRedirect(t *testing.T) {
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Store(true) }))
	defer target.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer proxy.Close()
	err := (App{}).restoreActorReady(t.Context(), strings.TrimPrefix(proxy.URL, "http://"))
	if err == nil || !strings.Contains(err.Error(), "HTTP status 302") || followed.Load() {
		t.Fatalf("readiness redirect followed or accepted: %v", err)
	}
}

func TestRestoreProxyClientDoesNotUseEnvironmentProxy(t *testing.T) {
	client := restoreProxyClient()
	defer client.CloseIdleConnections()
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || !transport.DisableKeepAlives {
		t.Fatal("proxy client can use an environment proxy or reuse a connection")
	}
}

func TestRestoreProxyStartsAndJoinsOnCancel(t *testing.T) {
	c := testConfig(t)
	r := prerequisiteRunner(func(ctx context.Context, cmd Command) ([]byte, error) {
		if cmd.Args[0] != "/client" || !strings.Contains(strings.Join(cmd.Args, " "), "--context "+c.AKS.ClusterName+" proxy actor") || cmd.StreamStderr == nil {
			t.Errorf("incorrect proxy command: %v", cmd.Args)
			return nil, errors.New("bad proxy command")
		}
		fmt.Fprintln(cmd.StreamStderr, "Proxying 127.0.0.1:12345 -> space/actor:80")
		<-ctx.Done()
		return nil, ctx.Err()
	})
	p, err := (App{Config: c, Runner: r}).startRestoreProxy(t.Context(), t.Context(), "/client", "space", "actor")
	if err != nil || p.address != "127.0.0.1:12345" {
		t.Fatalf("proxy=%v err=%v", p, err)
	}
	if joined, err := p.stop(t.Context()); !joined || err != nil {
		t.Fatalf("joined=%v error=%v", joined, err)
	}
}

func TestRestoreProxyUnconfirmedJoinIsDistinct(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p := &restoreProxy{cancel: func() {}, done: make(chan error)}
	joined, err := p.stop(ctx)
	if joined || !errors.Is(err, context.Canceled) {
		t.Fatalf("joined=%v error=%v", joined, err)
	}
}

func TestRestoreCleanupRetainsOnUnconfirmedProxyJoin(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	proxy := &restoreProxy{cancel: func() {}, done: make(chan error)}
	state := &restoreState{namespace: "space", proxy: proxy,
		actor:    restoreAPIIdentity{Name: "actor", Atespace: "space", UID: "actor-id"},
		template: restoreAPIIdentity{Name: "template", Atespace: "space", UID: "template-id"},
		pool:     restoreKubeIdentity{Name: "pool", Namespace: "space", UID: "pool-id"},
		ns:       restoreKubeIdentity{Name: "space", UID: "namespace-id"}}
	calls := 0
	runner := prerequisiteRunner(func(context.Context, Command) ([]byte, error) {
		calls++
		return nil, nil
	})
	err := (App{Config: testConfig(t), Runner: runner}).restoreCleanupWithin(ctx, state)
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("unconfirmed proxy join did not retain resources: error=%v calls=%d", err, calls)
	}
}

func TestRestoreCounterRequiresExactMemoryCount(t *testing.T) {
	for _, tc := range []struct {
		body         string
		status, want int
		ok           bool
	}{
		{"preserved memory count: 1 | preserved file counter: 1 | preserved rootfs counter: 1\n", 200, 1, true},
		{"preserved memory count: 2 | preserved file counter: 2 | preserved rootfs counter: 2\n", 200, 2, true},
		{"preserved memory count: 12 | preserved file counter: 1 | preserved rootfs counter: 1\n", 200, 1, false},
		{"preserved memory count: 1 | preserved file counter: 1 | preserved rootfs counter: 1\n", 500, 1, false},
		{"garbage", 200, 1, false},
		{strings.Repeat("x", 4097), 200, 1, false},
	} {
		t.Run(fmt.Sprint(tc.want, tc.status, tc.ok, len(tc.body)), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("method = %s", r.Method)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			err := restoreCounterCall(t.Context(), strings.TrimPrefix(srv.URL, "http://"), tc.want)
			if (err == nil) != tc.ok {
				t.Fatalf("unexpected result: %v", err)
			}
		})
	}
}

func TestRestoreCounterWholeReplyGrammar(t *testing.T) {
	reply := func(memory, file, root string) string {
		return "preserved memory count: " + memory + " | preserved file counter: " + file + " | preserved rootfs counter: " + root + "\n"
	}
	valid := reply("1", "-1", "9223372036854775807")
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"different counts", valid, true},
		{"signed extremes", reply("1", "-9223372036854775808", "0"), true},
		{"signed extremes reversed", reply("1", "9223372036854775807", "-9223372036854775808"), true},
		{"memory zero", reply("0", "0", "0"), false},
		{"memory maximum wrong count", reply("18446744073709551615", "0", "0"), false},
		{"memory overflow", reply("18446744073709551616", "0", "0"), false},
		{"memory negative", reply("-1", "0", "0"), false},
		{"memory plus", reply("+1", "0", "0"), false},
		{"memory leading zero", reply("01", "0", "0"), false},
		{"file positive overflow", reply("1", "9223372036854775808", "0"), false},
		{"file negative overflow", reply("1", "-9223372036854775809", "0"), false},
		{"root positive overflow", reply("1", "0", "9223372036854775808"), false},
		{"root negative overflow", reply("1", "0", "-9223372036854775809"), false},
		{"file plus", reply("1", "+1", "0"), false},
		{"root plus", reply("1", "0", "+1"), false},
		{"file negative zero", reply("1", "-0", "0"), false},
		{"root negative zero", reply("1", "0", "-0"), false},
		{"file leading zero", reply("1", "01", "0"), false},
		{"root leading zero", reply("1", "0", "-01"), false},
		{"missing fields", "preserved memory count: 1 | ", false},
		{"missing root", "preserved memory count: 1 | preserved file counter: 1\n", false},
		{"missing newline", strings.TrimSuffix(valid, "\n"), false},
		{"duplicate newline", valid + "\n", false},
		{"carriage return", strings.TrimSuffix(valid, "\n") + "\r\n", false},
		{"trailing content", strings.TrimSuffix(valid, "\n") + " | extra: 1\n", false},
		{"leading whitespace", " " + valid, false},
		{"altered label", strings.Replace(valid, "file counter", "file count", 1), false},
		{"altered separator", strings.Replace(valid, " | ", "|", 1), false},
		{"reordered", "preserved memory count: 1 | preserved rootfs counter: 0 | preserved file counter: 0\n", false},
		{"duplicate field", strings.TrimSuffix(valid, "\n") + " | preserved file counter: 1\n", false},
		{"empty memory", reply("", "0", "0"), false},
		{"empty file", reply("1", "", "0"), false},
		{"empty root", reply("1", "0", ""), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, tc.body) }))
			defer server.Close()
			err := restoreCounterCall(t.Context(), strings.TrimPrefix(server.URL, "http://"), 1)
			if (err == nil) != tc.ok {
				t.Fatalf("unexpected result: %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "not retried") {
				t.Fatalf("missing no-replay annotation: %v", err)
			}
			if !tc.ok && tc.name != "memory zero" && tc.name != "memory maximum wrong count" && !strings.Contains(err.Error(), "malformed") {
				t.Fatalf("missing malformed reason: %v", err)
			}
			if (tc.name == "memory zero" || tc.name == "memory maximum wrong count") && !strings.Contains(err.Error(), "expected 1, observed") {
				t.Fatalf("valid memory value was treated as malformed: %v", err)
			}
		})
	}
}

func TestRestoreRejectsMalformedCounterResponse(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, "preserved memory count: %d | ", count)
			}))
			defer server.Close()
			if err := restoreCounterCall(t.Context(), strings.TrimPrefix(server.URL, "http://"), count); err == nil {
				t.Fatal("accepted incomplete counter response")
			}
		})
	}
}
