package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/substrate-aks/internal/installer"
)

type defaultsRunner func(context.Context, installer.Command) ([]byte, error)

func (f defaultsRunner) Run(ctx context.Context, c installer.Command) ([]byte, error) {
	return f(ctx, c)
}

const (
	defaultSubscription = "22222222-2222-4222-8222-222222222222"
	defaultPublisher    = "33333333-3333-4333-8333-333333333333"
	defaultClientID     = "44444444-4444-4444-8444-444444444444"
)

func accountJSON(identityType string) string {
	return `{"id":"` + defaultSubscription + `","name":"Development","user":{"type":"` +
		identityType + `","name":"` + defaultClientID + `"}}`
}

func TestDiscoverAzureDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, subscription, publisher, identityType string
		wantCalls                                   [][]string
	}{
		{"signed-in user", "", "", "user", [][]string{
			{"az", "account", "show", "--only-show-errors", "--output", "json"},
			{"az", "ad", "signed-in-user", "show", "--only-show-errors", "--output", "json"},
		}},
		{"service principal in selected subscription", defaultSubscription, "", "servicePrincipal", [][]string{
			{"az", "account", "show", "--only-show-errors", "--output", "json"},
			{"az", "ad", "sp", "show", "--id", defaultClientID, "--only-show-errors", "--output", "json"},
		}},
		{"explicit publisher needs no directory lookup", "", defaultPublisher, "user", [][]string{
			{"az", "account", "show", "--only-show-errors", "--output", "json"},
		}},
		{"fully explicit needs no Azure access", defaultSubscription, defaultPublisher, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls [][]string
			runner := defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
				calls = append(calls, c.Args)
				if c.Args[1] == "account" {
					return []byte(accountJSON(tc.identityType)), nil
				}
				return []byte(`{"id":"` + defaultPublisher + `","appId":"` + defaultClientID + `","displayName":"Publisher"}`), nil
			})
			d, err := discoverAzureDefaults(t.Context(), tc.subscription, tc.publisher, runner)
			if err != nil || d.subscription != defaultSubscription || d.publisher != defaultPublisher {
				t.Fatalf("wrong discovered IDs (must use object ID, not client ID): %+v, %v", d, err)
			}
			if !reflect.DeepEqual(calls, tc.wantCalls) {
				t.Fatalf("unexpected discovery or lost target tenant: got %v, want %v", calls, tc.wantCalls)
			}
		})
	}
}

func TestDiscoverAzureDefaultsFailsExplicitly(t *testing.T) {
	blocked := errors.New("directory access denied")
	for _, tc := range []struct {
		name, account, identity, subscription string
		accountErr, identityErr               error
		want                                  string
	}{
		{name: "not signed in", accountErr: blocked, want: "az login"},
		{name: "bad account JSON", account: "{", want: "reading Azure CLI account"},
		{name: "missing subscription", account: "{}", want: "subscription ID"},
		{name: "wrong subscription", account: accountJSON("user"), subscription: "55555555-5555-4555-8555-555555555555", want: "unexpected subscription ID"},
		{name: "unknown authentication", account: accountJSON("unknown"), want: "--identity-object-id"},
		{name: "directory restricted", account: accountJSON("user"), identityErr: blocked, want: "--identity-object-id"},
		{name: "bad identity JSON", account: accountJSON("user"), identity: "{", want: "reading Azure identity"},
		{name: "client ID is not object ID", account: accountJSON("servicePrincipal"), identity: `{"appId":"` + defaultClientID + `"}`, want: "not the application/client ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
				if c.Args[1] == "account" {
					return []byte(tc.account), tc.accountErr
				}
				return []byte(tc.identity), tc.identityErr
			})
			d, err := discoverAzureDefaults(t.Context(), tc.subscription, "", runner)
			if err == nil || !strings.Contains(err.Error(), tc.want) || d.publisher != "" {
				t.Fatalf("discovery returned a silent or unsafe fallback: %+v, %v", d, err)
			}
			if (tc.accountErr != nil || tc.identityErr != nil) && !errors.Is(err, blocked) {
				t.Fatalf("lost underlying discovery error: %v", err)
			}
		})
	}
}

func TestSubscriptionOverrideDoesNotUseAnotherTenantOrIdentity(t *testing.T) {
	const target = "55555555-5555-4555-8555-555555555555"
	for _, change := range []string{"same identity", "different tenant", "different identity", "unknown tenant"} {
		t.Run(change, func(t *testing.T) {
			active := strings.Replace(accountJSON("user"), `"id":`, `"tenantId":"tenant-one","id":`, 1)
			selected := strings.Replace(active, defaultSubscription, target, 1)
			switch change {
			case "different tenant":
				selected = strings.Replace(selected, "tenant-one", "tenant-two", 1)
			case "different identity":
				selected = strings.Replace(selected, defaultClientID, "another-user", 1)
			case "unknown tenant":
				selected = strings.Replace(selected, `"tenantId":"tenant-one",`, "", 1)
			}
			var calls int
			runner := defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
				calls++
				switch calls {
				case 1:
					if strings.Contains(strings.Join(c.Args, " "), "--subscription") {
						t.Fatal("did not inspect the active directory context")
					}
					return []byte(active), nil
				case 2:
					if !strings.Contains(strings.Join(c.Args, " "), "--subscription "+target) {
						t.Fatal("did not resolve the overridden subscription")
					}
					return []byte(selected), nil
				default:
					if change != "same identity" {
						t.Fatal("queried the active directory for a different target identity")
					}
					return []byte(`{"id":"` + defaultPublisher + `"}`), nil
				}
			})
			d, err := discoverAzureDefaults(t.Context(), target, "", runner)
			if d.subscription != target {
				t.Fatalf("lost the overridden target: %+v", d)
			}
			if change == "same identity" {
				if err != nil || d.publisher != defaultPublisher {
					t.Fatalf("same-tenant override failed: %+v, %v", d, err)
				}
			} else if err == nil || d.publisher != "" || !strings.Contains(err.Error(), "--identity-object-id") {
				t.Fatalf("used an identity from the wrong target context: %+v, %v", d, err)
			}
		})
	}
}

func TestSetupDiscoversNewConfigAndPromptsOnlyForMissingIDs(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "discovered", true: "directory restricted"}[blocked], func(t *testing.T) {
			path := testRecord(t, "substrate-test")
			runner := defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
				if isRegionList(c) {
					return []byte(testRegionsJSON), nil
				}
				if c.Args[1] == "account" {
					return []byte(accountJSON("user")), nil
				}
				if blocked {
					return nil, errors.New("directory access denied")
				}
				return []byte(`{"id":"` + defaultPublisher + `","displayName":"Publisher"}`), nil
			})
			answers := "\n\nno\n\nyes\nno\n"
			if blocked {
				answers = defaultPublisher + "\n" + defaultPublisher + "\n" + answers
			}
			var output bytes.Buffer
			err := setupForTest(t.Context(), setupOptions{
				home: recordHome(path), name: recordName(path), cidr: "203.0.113.8/32", location: "eastus2", discovery: runner,
			}, strings.NewReader(answers), &output, &output, stopAtFirstStage)
			if !errors.Is(err, errFirstStage) {
				t.Fatalf("setup failed before displaying discovered target: %v\n%s", err, &output)
			}
			c, err := installer.LoadConfig(path)
			if err != nil || c.Azure.SubscriptionID != defaultSubscription || c.Azure.AssetPublisherObjectID != defaultPublisher {
				t.Fatalf("discovered or prompted identity was not saved: %+v, %v", c.Azure, err)
			}
			text := output.String()
			if !strings.Contains(text, "Development") || !strings.Contains(text, defaultSubscription) || !strings.Contains(text, defaultPublisher) ||
				strings.Contains(text, "Azure subscription UUID") ||
				strings.Contains(text, "Azure identity Entra object UUID") != blocked {
				t.Fatalf("setup lost discovery display or prompted for known IDs:\n%s", text)
			}
			if blocked && !strings.Contains(text, "directory access denied") {
				t.Fatalf("directory failure was hidden:\n%s", text)
			}
		})
	}
}

func TestSetupDiscoveryFailureInAutomationCreatesNoConfig(t *testing.T) {
	path := testRecord(t, "substrate-test")
	var output bytes.Buffer
	blocked := errors.New("directory access denied")
	err := setupForTest(t.Context(), setupOptions{
		home: recordHome(path), name: recordName(path), yes: true, discovery: defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
			if c.Args[1] == "account" {
				return []byte(accountJSON("user")), nil
			}
			return nil, blocked
		}),
	}, strings.NewReader(""), &output, &output, nil)
	if !errors.Is(err, blocked) {
		t.Fatalf("automation did not surface discovery failure: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("automation wrote a configuration despite missing publisher: %v", err)
	}
}

func TestSavedSubscriptionNameLookupFailureDoesNotRetarget(t *testing.T) {
	for _, name := range []string{"lookup failure", "wrong subscription"} {
		t.Run(name, func(t *testing.T) {
			path := testCLIConfig(t)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			err = setupForTest(t.Context(), setupOptions{
				home: recordHome(path), name: recordName(path), discovery: defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
					if isRegionList(c) {
						return []byte(testRegionsJSON), nil
					}
					if !strings.Contains(strings.Join(c.Args, " "), "--subscription "+defaultSubscription) {
						t.Fatal("metadata lookup did not target the saved subscription")
					}
					if name == "lookup failure" {
						return nil, errors.New("account unavailable")
					}
					return []byte(`{"id":"55555555-5555-4555-8555-555555555555","name":"Wrong target"}`), nil
				}),
			}, strings.NewReader("no\n"), &output, &output, nil)
			if err == nil || !strings.Contains(err.Error(), "before provisioning") {
				t.Fatalf("subscription metadata blocked target confirmation: %v", err)
			}
			text := strings.Join(strings.Fields(output.String()), " ")
			if !strings.Contains(text, "Could not load the subscription name") ||
				!strings.Contains(text, defaultSubscription) || strings.Contains(text, "Wrong target") {
				t.Fatalf("lookup silently failed or displayed a different target: %s", &output)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("metadata lookup rewrote installation configuration")
			}
		})
	}
}

func TestExistingSetupOnlyLooksUpConfiguredSubscriptionName(t *testing.T) {
	path := testCLIConfig(t)
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	calls := 0
	err = setupForTest(t.Context(), setupOptions{
		home: recordHome(path), name: recordName(path), discovery: defaultsRunner(func(ctx context.Context, c installer.Command) ([]byte, error) {
			if !isRegionList(c) {
				calls++
			}
			return subscriptionDisplayRunner(t).Run(ctx, c)
		}),
		ipClient: &http.Client{Transport: ipTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("existing config refreshed its API allowlist")
			return nil, nil
		})},
	}, strings.NewReader("no\n"), &output, &output, nil)
	if err == nil || !strings.Contains(err.Error(), "before provisioning") {
		t.Fatalf("unexpected setup result: %v", err)
	}
	last, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(first, last) {
		t.Fatal("existing setup was retargeted")
	}
	if calls != 1 || !strings.Contains(strings.Join(strings.Fields(output.String()), " "), "Development ("+defaultSubscription+")") {
		t.Fatalf("saved setup did not display its configured subscription name: calls=%d\n%s", calls, &output)
	}
}
