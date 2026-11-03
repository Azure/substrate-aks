package installer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("BUILDX_CONFIG", "")
	return Config{
		Installation: InstallationConfig{
			ID: "11111111-1111-4111-8111-111111111111", Name: "substrate-test", Dir: t.TempDir(),
			Ownership: Ownership{ResourceGroup: OwnershipCreated, Cluster: OwnershipCreated},
		},
		Azure: AzureConfig{
			SubscriptionID:         "22222222-2222-4222-8222-222222222222",
			AssetPublisherObjectID: "33333333-3333-4333-8333-333333333333",
			Location:               "eastus2", ResourceGroup: "substrate-test",
		},
		AKS: AKSConfig{
			ClusterName: "substrate-test", KubernetesVersion: "1.37",
			WorkerPool:         WorkerPoolConfig{VMSize: "Standard_D4s_v6", Count: 1},
			APIAuthorizedCIDRs: []string{"203.0.113.8/32"},
		},
		Substrate: SubstrateConfig{
			Source: Source{Repository: DefaultRepository, Commit: DefaultCommit},
			Images: ImagesConfig{
				Repository: "example.invalid/substrate", Tag: "experimental",
				Plugin: "example.invalid/azure-plugin@sha256:" + strings.Repeat("a", 64),
			},
		},
	}
}

func testInfrastructure() Infrastructure {
	return Infrastructure{
		ClusterName: "substrate-test", ResourceGroup: "substrate-test",
		StorageAccount: "substratetest123", AccountURL: "https://substratetest123.blob.core.windows.net",
		SnapshotsContainer: "snapshots", AssetsContainer: "assets",
		NodeClientID:    "44444444-4444-4444-8444-444444444444",
		ControlClientID: "55555555-5555-4555-8555-555555555555",
		TenantID:        "66666666-6666-4666-8666-666666666666",
	}
}

func TestConfigRejectsUnsafeInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"branch instead of SHA", func(c *Config) { c.Substrate.Source.Commit = "main" }},
		{"unrestricted API", func(c *Config) { c.AKS.APIAuthorizedCIDRs = []string{"0.0.0.0/0"} }},
		{"missing API ranges", func(c *Config) { c.AKS.APIAuthorizedCIDRs = nil }},
		{"contradictory API access", func(c *Config) { c.AKS.AllowUnrestrictedAPI = true }},
		{"wrong version", func(c *Config) { c.AKS.KubernetesVersion = "1.36" }},
		{"path in resource name", func(c *Config) { c.Azure.ResourceGroup = "../other" }},
		{"trailing hyphen in resource group", func(c *Config) { c.Azure.ResourceGroup = "substrate-" }},
		{"trailing hyphen in cluster name", func(c *Config) { c.AKS.ClusterName = "substrate-" }},
		{"credentials in source URL", func(c *Config) { c.Substrate.Source.Repository = "https://user:password@example.invalid/repo" }},
		{"missing publisher", func(c *Config) { c.Azure.AssetPublisherObjectID = "" }},
		{"invalid snapshot reader", func(c *Config) { c.Azure.SnapshotReaderObjectID = "not-an-object-id" }},
		{"invalid publisher type", func(c *Config) { c.Azure.AssetPublisherPrincipalType = "Client" }},
		{"invalid reader type", func(c *Config) {
			c.Azure.SnapshotReaderObjectID = c.Azure.AssetPublisherObjectID
			c.Azure.SnapshotReaderPrincipalType = "Client"
		}},
		{"reader type without identity", func(c *Config) { c.Azure.SnapshotReaderPrincipalType = "User" }},
		{"missing operator object ID", func(c *Config) { c.AKS.Operator = &OperatorConfig{PrincipalType: "User"} }},
		{"unsupported operator type", func(c *Config) {
			c.AKS.Operator = &OperatorConfig{ObjectID: c.Azure.AssetPublisherObjectID, PrincipalType: "clientID"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			tc.change(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
	c := testConfig(t)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Substrate.Images.Plugin = "example.invalid/plugin:latest"
	if err := c.ValidateInstall(); err == nil {
		t.Fatal("accepted mutable plugin image")
	}
}

func TestConfigNameBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"ab", false}, {"abc", true}, {"a-1", true},
		{"Uppercase", false}, {"under_score", false}, {"9cluster", false}, {"cluster-", false},
		{strings.Repeat("a", 40), true}, {strings.Repeat("a", 41), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			c.AKS.ClusterName, c.Azure.ResourceGroup = tc.name, tc.name
			if err := c.Validate(); (err == nil) != tc.ok {
				t.Fatalf("name validation: %v", err)
			}
		})
	}
}

func TestConfigRequiresExplicitUnrestrictedAPIOptIn(t *testing.T) {
	c := testConfig(t)
	c.AKS.APIAuthorizedCIDRs = nil
	c.AKS.AllowUnrestrictedAPI = true
	if err := c.Validate(); err != nil {
		t.Fatalf("explicit unrestricted access rejected: %v", err)
	}
}

func TestLoadGroupedConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy.json")
	data := `{
		"installation":{"id":"11111111-1111-4111-8111-111111111111","name":"substrate-test"},
		"azure":{
			"subscriptionId":"22222222-2222-4222-8222-222222222222",
			"assetPublisherObjectId":"33333333-3333-4333-8333-333333333333",
			"location":"southcentralus","resourceGroup":"substrate-test"
		},
		"aks":{"clusterName":"substrate-test","apiAuthorizedCidrs":["203.0.113.8/32"]},
		"substrate":{
			"images":{
				"repository":"example.invalid/substrate","tag":"experimental",
				"plugin":"example.invalid/plugin@sha256:` + strings.Repeat("a", 64) + `"
			},
			"runtime":{"assetsDir":"assets"}
		}
	}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateInstall(); err != nil {
		t.Fatal(err)
	}
	if c.Azure.Location != "southcentralus" || c.AKS.KubernetesVersion != "1.37" ||
		c.AKS.WorkerPool != (WorkerPoolConfig{VMSize: "Standard_D4s_v6", Count: 1}) ||
		c.Substrate.Source != (Source{Repository: DefaultRepository, Commit: DefaultCommit}) ||
		c.Installation.Dir != dir ||
		c.Installation.Ownership != (Ownership{ResourceGroup: OwnershipCreated, Cluster: OwnershipCreated}) ||
		c.Substrate.Runtime.AssetsDir != filepath.Join(dir, "assets") {
		t.Fatalf("grouped configuration lost settings, defaults or relative paths: %+v", c)
	}
}

func TestLoadConfigStrictAndRelativePaths(t *testing.T) {
	dir := t.TempDir()
	c := testConfig(t)
	c.Substrate.Runtime.AssetsDir = "assets"
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "deploy.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Installation.Dir != dir || loaded.Substrate.Runtime.AssetsDir != filepath.Join(dir, "assets") {
		t.Fatalf("relative paths did not resolve against configuration directory: %+v", loaded)
	}
	for _, bad := range [][]byte{
		append(append([]byte{}, data...), []byte("\n{}")...),
		[]byte(strings.Replace(string(data), `"count":1`, `"counnt":1`, 1)),
	} {
		if err := os.WriteFile(path, bad, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatalf("accepted invalid document: %s", bad)
		}
	}
}

func TestCleanEnvironment(t *testing.T) {
	preserved := []string{
		"PATH=/bin", "HOME=/home/test", "AZURE_CONFIG_DIR=/azure", "GOTOOLCHAIN=auto",
		"AZURE_CLIENT_ID=test-client", "DOCKER_CONFIG=/docker", "DOCKER_HOST=tcp://builder.invalid:2376",
		"DOCKER_CONTEXT=remote", "BUILDX_CONFIG=/buildx", "REGISTRY_AUTH_FILE=/registry/auth.json",
	}
	input := append(slices.Clone(preserved),
		"ATE_CONFIG=other.yaml", "ATE_INSTALL_KIND=true", "VERSION=old",
		"PROJECT_ID=other", "KUBECTL_CONTEXT=other", "AWS_ACCESS_KEY_ID=secret",
		"ARCH=arm64", "OUT=/other-assets", "KATA_VER=other", "CH_VER=other",
		"ACTOR_JWT_ALGORITHM=RS256", "ACTOR_JWT_ISSUER=https://caller.invalid",
		"EXPECTED_JWT_ISSUER=https://caller.invalid", "DOCKER_BUILD_FLAGS=--no-cache",
		"E2E_ATE_ENVOY_CONCURRENCY=4", "KIND_CLUSTER_NAME=other",
		"BENCHMARK_ACTOR_MEMORY=1Gi", "ANTHROPIC_API_KEY=synthetic-test-value",
	)
	got := cleanEnvironment(input)
	if !slices.Equal(got, preserved) {
		t.Fatalf("inherited conflicting settings: %v", got)
	}
}
