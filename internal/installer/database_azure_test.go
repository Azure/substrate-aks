//go:build integration

package installer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Azure/substrate-aks/infra"
)

var vaultContractSubscription = flag.String("vault-contract-subscription", "", "subscription for explicitly authorized disposable Key Vault tests")
var vaultContractOperator = flag.String("vault-contract-operator", "", "Entra object ID of the signed-in operator for disposable Key Vault tests")
var databaseRegionSubscription = flag.String("database-region-subscription", "", "subscription for read-only managed PostgreSQL capability tests")

func TestAzureDatabaseRegionCapabilities(t *testing.T) {
	if !uuidPattern.MatchString(*databaseRegionSubscription) {
		t.Fatal("provide -database-region-subscription; this test only reads Flexible Server westus3 and HorizonDB eastus capabilities")
	}
	for _, backend := range []DatabaseBackend{DatabaseFlexible, DatabaseHorizon} {
		t.Run(string(backend), func(t *testing.T) {
			c, _ := managedTestConfig(t, backend)
			if err := ValidateDatabaseRegion(t.Context(), ExecRunner{}, *databaseRegionSubscription, c.Azure.Location, c.Database); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAzureDatabaseRegionDiscovery(t *testing.T) {
	if !uuidPattern.MatchString(*databaseRegionSubscription) {
		t.Fatal("provide -database-region-subscription; this test only discovers advertised managed database regions")
	}
	runner := ExecRunner{}
	a := App{Config: Config{Azure: AzureConfig{SubscriptionID: *databaseRegionSubscription}}, Runner: runner}
	data, err := a.az(t.Context(), "rest", "--method", "get", "--url",
		"https://management.azure.com/subscriptions/"+*databaseRegionSubscription+"/locations?api-version=2022-12-01",
		"--query", "value[?metadata.regionType=='Physical'].name")
	if err != nil {
		t.Fatal(err)
	}
	var regions []string
	if err := json.Unmarshal(data, &regions); err != nil || len(regions) == 0 {
		t.Fatalf("reading physical region catalog: %v", err)
	}
	for _, backend := range []DatabaseBackend{DatabaseFlexible, DatabaseHorizon} {
		t.Run(string(backend), func(t *testing.T) {
			available, err := AvailableDatabaseRegions(t.Context(), runner, *databaseRegionSubscription, backend, regions)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("subscription-supported %s regions: %v", backend, available)
		})
	}
}

func TestAzureDatabaseVaultContract(t *testing.T) {
	if !uuidPattern.MatchString(*vaultContractSubscription) || !uuidPattern.MatchString(*vaultContractOperator) {
		t.Fatal("provide -vault-contract-subscription and -vault-contract-operator; this test creates and deletes two disposable Key Vault resource groups, but no AKS or database")
	}
	for _, backend := range []DatabaseBackend{DatabaseFlexible, DatabaseHorizon} {
		t.Run(string(backend), func(t *testing.T) {
			c, _ := managedTestConfig(t, backend)
			c.Azure.SubscriptionID = *vaultContractSubscription
			c.Azure.AssetPublisherObjectID = *vaultContractOperator
			// The vault grant goes to the AKS operator, so the signed-in user must be it to read back credentials.
			c.AKS.Operator = &OperatorConfig{ObjectID: *vaultContractOperator, PrincipalType: "User"}
			c.Azure.Location = "eastus"
			c.Azure.ResourceGroup = fmt.Sprintf("subpg-contract-%d", time.Now().UnixNano())
			id := make([]byte, 16)
			if _, err := rand.Read(id); err != nil {
				t.Fatal(err)
			}
			id[6], id[8] = (id[6]&0x0f)|0x40, (id[8]&0x3f)|0x80
			c.Installation.ID = fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			var log bytes.Buffer
			a := App{Config: c, Runner: ExecRunner{Log: &log, Verbose: &log}}
			data, err := a.az(t.Context(), "group", "exists", "--name", c.Azure.ResourceGroup)
			if err != nil || string(bytes.TrimSpace(data)) != "false" {
				t.Fatal("disposable resource group is not confirmed absent; refusing to proceed")
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()
				if err := a.ownedGroup(ctx, true); errors.Is(err, errGroupAbsent) {
					return
				} else if err != nil {
					t.Errorf("cannot confirm disposable group ownership for cleanup: %v", err)
					return
				}
				if err := a.Destroy(ctx, c.Azure.ResourceGroup); err != nil {
					t.Errorf("deleting disposable vault group %s: %v", c.Azure.ResourceGroup, err)
					return
				}
				if _, err := a.az(ctx, "deployment", "sub", "delete", "--name", c.deploymentName()+"-vault"); err != nil {
					t.Errorf("deleting disposable vault deployment record: %v", err)
				}
			})
			dir := filepath.Join(c.workspace(), "infra")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"database-vault.bicep", "vault.bicep"} {
				data, err := infra.Files.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.provisionDatabaseVault(t.Context(), dir); err != nil {
				t.Fatal(err)
			}
			var first databaseSecrets
			deadline := time.Now().Add(3 * time.Minute)
			for {
				first, err = a.databaseCredentials(t.Context(), true)
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("vault credentials were not usable after RBAC propagation wait: %v", err)
				}
				select {
				case <-t.Context().Done():
					t.Fatal(t.Context().Err())
				case <-time.After(5 * time.Second):
				}
			}
			// A separate workspace must recover the same credentials from the vault.
			retry := a
			retry.Config.Installation.Dir = t.TempDir()
			second, err := retry.databaseCredentials(t.Context(), false)
			if err != nil || first != second {
				t.Fatalf("fresh workspace could not reuse vault credentials: %v", err)
			}
			template := `{"$schema":"https://schema.management.azure.com/schemas/2019-04-01/deploymentTemplate.json#","contentVersion":"1.0.0.0","parameters":{"databaseCredentials":{"type":"secureString"},"backend":{"type":"string"}},"resources":[],"outputs":{"bindingMatches":{"type":"bool","value":"[and(equals(json(parameters('databaseCredentials')).backend, parameters('backend')), greater(length(json(parameters('databaseCredentials')).admin), 0))]"}}}`
			templatePath := filepath.Join(dir, "reference-contract.json")
			if err := os.WriteFile(templatePath, []byte(template), 0600); err != nil {
				t.Fatal(err)
			}
			params, err := json.Marshal(map[string]any{"parameters": map[string]any{
				"backend": map[string]any{"value": backend},
				"databaseCredentials": map[string]any{"reference": map[string]any{
					"keyVault": map[string]string{"id": c.databaseVaultID()}, "secretName": databaseSecretName,
				}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			data, err = a.Runner.Run(t.Context(), Command{
				Args: []string{"az", "deployment", "group", "create", "--name", "vault-reference-contract",
					"--resource-group", c.Azure.ResourceGroup, "--template-file", templatePath,
					"--parameters", "@/dev/stdin", "--query", "properties.outputs.bindingMatches.value",
					"--subscription", c.Azure.SubscriptionID, "--only-show-errors", "--output", "json"},
				Input: params, Env: databaseCommandEnvironment, SensitiveOutput: true,
			})
			if err != nil || string(bytes.TrimSpace(data)) != "true" {
				t.Fatalf("ARM could not resolve and parse the secure Key Vault reference: %v", err)
			}
			if err := filepath.WalkDir(c.workspace(), func(path string, entry os.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				for _, password := range []string{first.Admin, first.Owner, first.ReadWrite} {
					if bytes.Contains(data, []byte(password)) {
						return fmt.Errorf("database credential persisted in %s", path)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, password := range []string{first.Admin, first.Owner, first.ReadWrite} {
				if bytes.Contains(log.Bytes(), []byte(password)) {
					t.Fatal("database credential leaked into installer diagnostics")
				}
			}
		})
	}
}
