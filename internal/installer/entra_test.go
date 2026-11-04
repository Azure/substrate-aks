package installer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func entraKubeconfigFixture(name string) []byte {
	return []byte(strings.Replace(string(kubeconfigFixture(name)),
		`user: {token: "synthetic-token"}`,
		`user: {exec: {command: kubelogin, apiVersion: client.authentication.k8s.io/v1beta1, args: [get-token, --login, azurecli]}}`, 1))
}

func TestEntraCredentialsConvertPrivatelyAndPreserveOldFileOnFailure(t *testing.T) {
	for _, mode := range []string{"success", "conversion failure", "local credentials returned"} {
		t.Run(mode, func(t *testing.T) {
			c := testConfig(t)
			c.AKS.Operator = &OperatorConfig{ObjectID: c.Azure.AssetPublisherObjectID, PrincipalType: "User"}
			a := App{Config: c}
			if err := os.MkdirAll(c.workspace(), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(a.kubeconfig(), []byte("previous credentials"), 0600); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("conversion failed")
			var conversionErr error
			if mode == "conversion failure" {
				conversionErr = failure
			}
			a.Runner = script(t, reply{contains: "aks get-credentials"},
				reply{contains: "kubelogin convert-kubeconfig --login azurecli --kubeconfig", effect: func(cmd Command) {
					if mode == "success" {
						path := cmd.Args[len(cmd.Args)-1]
						if path == a.kubeconfig() {
							t.Fatal("conversion changed working credentials in place")
						}
						if err := os.WriteFile(path, entraKubeconfigFixture(c.AKS.ClusterName), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}, err: conversionErr})
			err := a.credentials(t.Context())
			if (err == nil) != (mode == "success") || mode == "conversion failure" && !errors.Is(err, failure) {
				t.Fatalf("conversion outcome = %v", err)
			}
			data, readErr := os.ReadFile(a.kubeconfig())
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode == "success" {
				if err := validateEntraKubeconfig(data, c.AKS.ClusterName); err != nil {
					t.Fatal(err)
				}
			} else if string(data) != "previous credentials" {
				t.Fatal("failed conversion replaced working credentials")
			}
			leftovers, err := filepath.Glob(filepath.Join(c.workspace(), ".kubeconfig-*"))
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("temporary credentials retained: %v, %v", leftovers, err)
			}
		})
	}
}

func TestProvisionOperatorParametersAndLegacyCompatibility(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "managed Entra"}[managed], func(t *testing.T) {
			c := testConfig(t)
			if managed {
				c.AKS.Operator = &OperatorConfig{ObjectID: "77777777-7777-4777-8777-777777777777", PrincipalType: "ServicePrincipal"}
			}
			r := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "false"},
				reply{contains: "deployment sub what-if", output: `{"status":"Succeeded","changes":[]}`})
			if err := (App{Config: c, Runner: r}).Provision(t.Context(), true); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(c.workspace(), "infra/parameters.json"))
			if err != nil {
				t.Fatal(err)
			}
			var parameters struct {
				Parameters map[string]struct{ Value any }
			}
			if err := json.Unmarshal(data, &parameters); err != nil {
				t.Fatal(err)
			}
			if managed {
				if parameters.Parameters["operatorObjectId"].Value != c.AKS.Operator.ObjectID ||
					parameters.Parameters["operatorPrincipalType"].Value != "ServicePrincipal" {
					t.Fatal("Bicep received publisher credentials instead of the configured operator")
				}
			} else if _, present := parameters.Parameters["operatorObjectId"]; present {
				t.Fatal("legacy configuration silently enabled managed Entra")
			}
		})
	}
}

func TestOperatorAccessDoesNotMaskNetworkOrAuthenticationFailure(t *testing.T) {
	c := testConfig(t)
	failure := errors.New("API connection refused")
	r := script(t, reply{contains: "create -f - --output=json", err: failure})
	if err := (App{Config: c, Runner: r}).waitOperatorAccess(t.Context()); !errors.Is(err, failure) {
		t.Fatalf("network failure was hidden as RBAC propagation: %v", err)
	}
	for _, reason := range []string{"", "Azure does not have an opinion for this user"} {
		t.Run("denied/"+reason, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			output, err := json.Marshal(map[string]any{"status": map[string]any{"allowed": false, "reason": reason}})
			if err != nil {
				t.Fatal(err)
			}
			r := script(t, reply{contains: "create -f - --output=json", output: string(output),
				effect: func(Command) { cancel() }})
			if err := (App{Config: c, Runner: r}).waitOperatorAccess(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("RBAC denial was not retried or ignored cancellation: %v", err)
			}
		})
	}
}

func TestOperatorAccessUsesStructuredAuthorizationReview(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		ok           bool
	}{
		{"allowed", `{"status":{"allowed":true}}`, true},
		{"missing status", `{}`, false},
		{"invalid JSON", `no - denied`, false},
		{"evaluation failure", `{"status":{"allowed":false,"evaluationError":"webhook unavailable"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := script(t, reply{contains: "create -f - --output=json", output: tc.output, effect: func(cmd Command) {
				var review struct {
					Kind string
					Spec struct {
						ResourceAttributes struct{ Group, Resource, Verb string }
					}
				}
				if err := json.Unmarshal(cmd.Input, &review); err != nil {
					t.Fatal(err)
				}
				attributes := review.Spec.ResourceAttributes
				if review.Kind != "SelfSubjectAccessReview" || attributes.Group != "apiextensions.k8s.io" ||
					attributes.Resource != "customresourcedefinitions" || attributes.Verb != "create" {
					t.Fatalf("authorization check tests the wrong permission: %s", cmd.Input)
				}
			}})
			if err := (App{Config: testConfig(t), Runner: r}).waitOperatorAccess(t.Context()); (err == nil) != tc.ok {
				t.Fatalf("authorization outcome: %v", err)
			}
		})
	}
}
