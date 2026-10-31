package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/Azure/substrate-aks/internal/installer"
)

type ipTransport func(*http.Request) (*http.Response, error)

func (f ipTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResolveAPICIDR(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		valid      bool
	}{
		{"public IPv4", "203.0.113.8\n", 200, true},
		{"IPv6", "2001:db8::1", 200, false},
		{"private", "10.0.0.1", 200, false},
		{"loopback", "127.0.0.1", 200, false},
		{"link local", "169.254.1.1", 200, false},
		{"not an address", "<html>unavailable</html>", 200, false},
		{"oversized", "203.0.113.8" + strings.Repeat(" ", 65) + "junk", 200, false},
		{"service failure", "unavailable", 503, false},
		{"redirect", "203.0.113.8", 302, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: ipTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != "https://api.ipify.org" || r.Method != http.MethodGet {
					t.Fatalf("unexpected public IP lookup: %s %s", r.Method, r.URL)
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			cidr, err := resolveAPICIDR(t.Context(), "", false, client)
			if tc.valid {
				if err != nil || cidr != "203.0.113.8/32" {
					t.Fatalf("lookup did not produce the caller-only range: %q, %v", cidr, err)
				}
			} else if err == nil || cidr != "" || !strings.Contains(err.Error(), "--api-cidr") {
				t.Fatalf("lookup failure produced unsafe or silent access: %q, %v", cidr, err)
			}
		})
	}
}

func TestAPIAccessOverridesSkipLookup(t *testing.T) {
	client := &http.Client{Transport: ipTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("explicit API access contacted the IP discovery service")
		return nil, nil
	})}
	for _, tc := range []struct {
		cidr         string
		unrestricted bool
		fail         bool
	}{
		{"198.51.100.0/24", false, false},
		{"", true, false},
		{"198.51.100.8/32", true, true},
	} {
		cidr, err := resolveAPICIDR(t.Context(), tc.cidr, tc.unrestricted, client)
		if tc.fail {
			if err == nil {
				t.Fatal("contradictory flags accepted")
			}
		} else if err != nil || cidr != tc.cidr {
			t.Fatalf("explicit access changed: %q, %v", cidr, err)
		}
	}
}

func TestAPILookupPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client := &http.Client{Transport: ipTransport(func(r *http.Request) (*http.Response, error) {
		return nil, r.Context().Err()
	})}
	if _, err := resolveAPICIDR(ctx, "", false, client); !errors.Is(err, context.Canceled) {
		t.Fatalf("lookup lost cancellation: %v", err)
	}
}

func TestSetupIPDiscoveryFailureCannotOpenAccessInAutomation(t *testing.T) {
	path := testRecord(t, "substrate-test")
	blocked := errors.New("IP lookup unavailable")
	var output bytes.Buffer
	err := setupForTest(t.Context(), setupOptions{
		home: recordHome(path), name: recordName(path), subscription: defaultSubscription, publisher: defaultPublisher, yes: true,
		ipClient: &http.Client{Transport: ipTransport(func(*http.Request) (*http.Response, error) {
			return nil, blocked
		})},
	}, strings.NewReader(""), &output, &output, nil)
	if !errors.Is(err, blocked) || !strings.Contains(err.Error(), "--api-cidr") {
		t.Fatalf("lookup failure was hidden: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed lookup wrote a configuration: %v", err)
	}
}

func TestSetupAPIAccessDefaultsAndFallback(t *testing.T) {
	for _, name := range []string{"detected", "explicit", "unrestricted", "lookup failure"} {
		t.Run(name, func(t *testing.T) {
			cidr := ""
			if name == "explicit" {
				cidr = "198.51.100.0/24"
			}

			path := testRecord(t, "substrate-test")
			answers := "\n\nno\nno\n"
			if name == "lookup failure" {
				answers = "203.0.113.8/32\n" + answers
			}
			var output bytes.Buffer
			err := setupForTest(t.Context(), setupOptions{
				home: recordHome(path), name: recordName(path), subscription: defaultSubscription, publisher: defaultPublisher,
				discovery: subscriptionDisplayRunner(t),
				cidr:      cidr, location: "eastus2", unrestrictedAPI: name == "unrestricted",
				ipClient: &http.Client{Transport: ipTransport(func(*http.Request) (*http.Response, error) {
					if name == "explicit" || name == "unrestricted" {
						t.Fatal("explicit access triggered discovery")
					}
					if name == "lookup failure" {
						return nil, errors.New("IP lookup unavailable")
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("203.0.113.8"))}, nil
				})},
			}, strings.NewReader(answers), &output, &output, func(context.Context, []string, io.Writer, io.Writer) error {
				t.Fatal("setup continued after billing was declined")
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "before provisioning") {
				t.Fatalf("setup did not reach the target confirmation: %v\n%s", err, &output)
			}
			c, err := installer.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if name == "unrestricted" {
				if !c.AKS.AllowUnrestrictedAPI || len(c.AKS.APIAuthorizedCIDRs) != 0 ||
					!strings.Contains(strings.Join(strings.Fields(output.String()), " "), "WARNING: "+unrestrictedAPIWarning) {
					t.Fatalf("unrestricted access lost explicit intent or warning: %+v\n%s", c.AKS, &output)
				}
			} else {
				want := "203.0.113.8/32"
				if name == "explicit" {
					want = cidr
				}
				if c.AKS.AllowUnrestrictedAPI || len(c.AKS.APIAuthorizedCIDRs) != 1 || c.AKS.APIAuthorizedCIDRs[0] != want {
					t.Fatalf("setup did not preserve safe API access: %+v", c.AKS)
				}
			}
			if strings.Contains(output.String(), "Client public IPv4 CIDR allowed") != (name == "lookup failure") {
				t.Fatalf("setup asked for a known CIDR or failed to ask after lookup failure:\n%s", &output)
			}
		})
	}
}
