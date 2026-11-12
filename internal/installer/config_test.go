package installer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/substrate-aks/infra"
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
			SystemPool:         SystemPoolConfig{VMSize: DefaultSystemVMSize, Count: 1},
			WorkerPool:         WorkerPoolConfig{VMSize: "Standard_D4s_v6", Count: 1, OSDiskType: "Managed", OSDiskSizeGB: 128},
			APIAuthorizedCIDRs: []string{"203.0.113.8/32"},
		},
		Substrate: SubstrateConfig{
			Source:       DefaultConfig().Substrate.Source,
			PluginSource: DefaultConfig().Substrate.PluginSource,
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
		{"plugin branch instead of SHA", func(c *Config) { c.Substrate.PluginSource.Commit = "main" }},
		{"plugin repository with credentials", func(c *Config) {
			c.Substrate.PluginSource.Repository = "https://user:token@github.com/example/plugin.git"
		}},
		{"non-release source tag", func(c *Config) { c.Substrate.Source.Tag = "main" }},
		{"unknown image source", func(c *Config) { c.Substrate.ImageSource = "ghcr" }},
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

func TestConfigWorkerPoolBoundaries(t *testing.T) {
	for _, tc := range []struct {
		size  string
		count int
		ok    bool
	}{
		{"Standard_D8s_v7", 0, false}, {"Standard_D8s_v7", 1, true}, {"Standard_D8s_v7", 32, true},
		{"Standard_D8s_v7", MaxWorkerCount, true}, {"Standard_D8s_v7", MaxWorkerCount + 1, false},
		{"D8s_v7", 1, false}, {"Standard_D8s v7", 1, false},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.size, tc.count), func(t *testing.T) {
			c := testConfig(t)
			c.AKS.WorkerPool = WorkerPoolConfig{VMSize: tc.size, Count: tc.count}
			if err := c.Validate(); (err == nil) != tc.ok {
				t.Fatalf("worker pool validation: %v", err)
			}
		})
	}
}

func TestConfigSystemPoolBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name           string
		size           string
		system, worker int
		ok             bool
	}{
		{"default", DefaultSystemVMSize, 1, 1, true},
		{"zero count", DefaultSystemVMSize, 0, 1, false},
		{"negative count", DefaultSystemVMSize, -1, 1, false},
		{"bad size", "D4s_v6", 1, 1, false},
		{"unsafe size", "Standard_D4s v6", 1, 1, false},
		{"overlay limit", DefaultSystemVMSize, 154, 100, true},
		{"overlay overflow", DefaultSystemVMSize, 155, 100, false},
		{"large count", DefaultSystemVMSize, int(^uint(0) >> 1), 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			c.AKS.SystemPool = SystemPoolConfig{VMSize: tc.size, Count: tc.system}
			c.AKS.WorkerPool.Count = tc.worker
			if err := c.Validate(); (err == nil) != tc.ok {
				t.Fatalf("system pool validation: %v", err)
			}
		})
	}
}

func TestConfigWorkerDiskAndPodBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*WorkerPoolConfig)
		ok     bool
	}{
		{"defaults", func(*WorkerPoolConfig) {}, true},
		{"unset", func(w *WorkerPoolConfig) { *w = WorkerPoolConfig{VMSize: w.VMSize, Count: 1} }, true},
		{"ephemeral", func(w *WorkerPoolConfig) { w.OSDiskType = "Ephemeral" }, true},
		{"lowercase type", func(w *WorkerPoolConfig) { w.OSDiskType = "ephemeral" }, false},
		{"disk SKU as type", func(w *WorkerPoolConfig) { w.OSDiskType = "Premium_LRS" }, false},
		{"smallest disk", func(w *WorkerPoolConfig) { w.OSDiskSizeGB = 30 }, true},
		{"disk too small", func(w *WorkerPoolConfig) { w.OSDiskSizeGB = 29 }, false},
		{"largest disk", func(w *WorkerPoolConfig) { w.OSDiskSizeGB = 2048 }, true},
		{"disk too large", func(w *WorkerPoolConfig) { w.OSDiskSizeGB = 2049 }, false},
		{"fewest pods", func(w *WorkerPoolConfig) { w.MaxPods, w.Count = 10, 3 }, true},
		{"pool below the AKS minimum", func(w *WorkerPoolConfig) { w.MaxPods, w.Count = 10, 2 }, false},
		{"one node at the AKS minimum", func(w *WorkerPoolConfig) { w.MaxPods = 30 }, true},
		{"one node below the AKS minimum", func(w *WorkerPoolConfig) { w.MaxPods = 29 }, false},
		{"too few pods", func(w *WorkerPoolConfig) { w.MaxPods = 9 }, false},
		{"most pods", func(w *WorkerPoolConfig) { w.MaxPods = 250 }, true},
		{"too many pods", func(w *WorkerPoolConfig) { w.MaxPods = 251 }, false},
		{"largest ephemeral disk", func(w *WorkerPoolConfig) { w.OSDiskType, w.OSDiskSizeGB = "Ephemeral", 2040 }, true},
		{"ephemeral disk over Azure's cap", func(w *WorkerPoolConfig) { w.OSDiskType, w.OSDiskSizeGB = "Ephemeral", 2041 }, false},
		{"largest managed disk is too large for ephemeral", func(w *WorkerPoolConfig) { w.OSDiskType, w.OSDiskSizeGB = "Ephemeral", 2048 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			tc.change(&c.AKS.WorkerPool)
			if err := c.Validate(); (err == nil) != tc.ok {
				t.Fatalf("worker disk or pod validation: %v", err)
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
		c.AKS.SystemPool != (SystemPoolConfig{VMSize: DefaultSystemVMSize, Count: 1}) ||
		c.AKS.WorkerPool != (WorkerPoolConfig{VMSize: "Standard_D4s_v6", Count: 1, OSDiskType: "Managed", OSDiskSizeGB: 128}) ||
		c.Substrate.Source != (Source{Repository: DefaultRepository, Tag: DefaultTag, Commit: DefaultCommit}) ||
		c.Substrate.PluginSource != (Source{Repository: DefaultPluginRepository, Commit: DefaultPluginCommit}) ||
		c.Substrate.ImageSource != "" ||
		c.Installation.Dir != dir ||
		c.Installation.Ownership != (Ownership{ResourceGroup: OwnershipCreated, Cluster: OwnershipCreated}) ||
		c.Substrate.Runtime.AssetsDir != filepath.Join(dir, "assets") {
		t.Fatalf("grouped configuration lost settings, defaults or relative paths: %+v", c)
	}
}

func TestLoadMainEraRecordPlanDestroyAndResave(t *testing.T) {
	data, err := os.ReadFile("testdata/installation-main-era.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "installation.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.AKS.SystemPool != (SystemPoolConfig{VMSize: DefaultSystemVMSize, Count: 1}) {
		t.Fatalf("old record lost system defaults: %+v", c.AKS.SystemPool)
	}
	plan := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "true"},
		ownerReply(c), clusterReply(c), operatorRecordReply(c),
		reply{contains: "deployment sub what-if", output: `{"status":"Succeeded","changes":[]}`})
	if err := (App{Config: c, Runner: plan}).Provision(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	destroy := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "true"},
		ownerReply(c), reply{contains: "group delete"})
	if err := (App{Config: c, Runner: destroy}).Destroy(t.Context(), c.Azure.ResourceGroup); err != nil {
		t.Fatal(err)
	}
	result := BuildResult{Images: testConfig(t).Substrate.Images, AssetsDir: "/test/assets"}
	if err := SaveBuildResult(path, c, result); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var serialized struct {
		AKS struct {
			SystemPool *struct {
				VMSize *string `json:"vmSize"`
				Count  *int    `json:"count"`
			} `json:"systemPool"`
		} `json:"aks"`
	}
	if err := json.Unmarshal(saved, &serialized); err != nil {
		t.Fatal(err)
	}
	pool := serialized.AKS.SystemPool
	if pool == nil || pool.VMSize == nil || *pool.VMSize != DefaultSystemVMSize || pool.Count == nil || *pool.Count != 1 {
		t.Fatalf("image re-save did not persist system defaults: %s", saved)
	}
	reloaded, err := LoadConfig(path)
	if err != nil || reloaded.AKS.SystemPool != c.AKS.SystemPool {
		t.Fatalf("re-saved record lost defaults: %+v, %v", reloaded.AKS.SystemPool, err)
	}
}

func TestLoadSystemPoolFieldsAndStrictDecoding(t *testing.T) {
	record, err := os.ReadFile("testdata/installation-main-era.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, system string
		want         SystemPoolConfig
		ok           bool
	}{
		{"omitted", "", SystemPoolConfig{DefaultSystemVMSize, 1}, true},
		{"size only", `"systemPool":{"vmSize":"Standard_F4s_v6"},`, SystemPoolConfig{"Standard_F4s_v6", 1}, true},
		{"count only", `"systemPool":{"count":3},`, SystemPoolConfig{DefaultSystemVMSize, 3}, true},
		{"explicit zero", `"systemPool":{"count":0},`, SystemPoolConfig{}, false},
		{"unknown field", `"systemPool":{"count":1,"unknown":true},`, SystemPoolConfig{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "installation.json")
			data := strings.Replace(string(record), `"workerPool":`, tc.system+`"workerPool":`, 1)
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := LoadConfig(path)
			if (err == nil) != tc.ok || tc.ok && c.AKS.SystemPool != tc.want {
				t.Fatalf("system pool decode: %+v, %v", c.AKS.SystemPool, err)
			}
		})
	}
}

func TestLoadSystemPoolImageResavePreservesOverride(t *testing.T) {
	c := testConfig(t)
	c.AKS.SystemPool = SystemPoolConfig{VMSize: "Standard_F4s_v6", Count: 3}
	path := filepath.Join(c.workspace(), "installation.json")
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	expected, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveBuildResult(path, expected, BuildResult{Images: c.Substrate.Images, AssetsDir: "/test/assets"}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil || got.AKS.SystemPool != c.AKS.SystemPool {
		t.Fatalf("image re-save changed configured system pool: %+v, %v", got.AKS.SystemPool, err)
	}
}

func TestConfigSystemPoolBicepMapping(t *testing.T) {
	for _, tc := range []struct {
		file  string
		parts []string
	}{
		{"main.bicep", []string{
			"param systemVmSize string = 'Standard_D4s_v6'", "@minValue(1)\nparam systemCount int = 1",
			"systemVmSize: systemVmSize", "systemCount: systemCount",
		}},
		{"resources.bicep", []string{
			"param systemVmSize string = 'Standard_D4s_v6'", "@minValue(1)\nparam systemCount int = 1",
			"name: 'system'\n        mode: 'System'\n        count: systemCount\n        vmSize: systemVmSize",
		}},
	} {
		t.Run(tc.file, func(t *testing.T) {
			data, err := infra.Files.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			for _, part := range tc.parts {
				if !strings.Contains(string(data), part) {
					t.Fatalf("%s missing system pool default or mapping %q", tc.file, part)
				}
			}
		})
	}
}

// The expected bytes were captured from a fake plan at main commit b5f2b97
// using the same parameter-affecting values as the main-era record fixture.
func TestConfigOmittedSystemPoolMatchesMainParameters(t *testing.T) {
	record, err := os.ReadFile("testdata/installation-main-era.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "installation.json")
	if err := os.WriteFile(path, record, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	r := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "false"},
		reply{contains: "deployment sub what-if", output: `{"status":"Succeeded","changes":[]}`})
	if err := (App{Config: c, Runner: r}).Provision(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(c.workspace(), "infra", "parameters.json"))
	if err != nil {
		t.Fatal(err)
	}
	const mainParameters = `{"$schema":"https://schema.management.azure.com/schemas/2019-04-01/deploymentParameters.json#","contentVersion":"1.0.0.0","parameters":{"apiAuthorizedCidrs":{"value":["203.0.113.8/32"]},"assetPublisherObjectId":{"value":"33333333-3333-4333-8333-333333333333"},"assetPublisherPrincipalType":{"value":""},"clusterName":{"value":"substrate-test"},"installationId":{"value":"11111111-1111-4111-8111-111111111111"},"kubernetesVersion":{"value":"1.37"},"location":{"value":"eastus2"},"registryOnly":{"value":false},"resourceGroupName":{"value":"substrate-test"},"snapshotReaderObjectId":{"value":""},"snapshotReaderPrincipalType":{"value":""},"workerCount":{"value":1},"workerMaxPods":{"value":0},"workerOsDiskSizeGB":{"value":128},"workerOsDiskType":{"value":"Managed"},"workerVmSize":{"value":"Standard_D4s_v6"}}}`
	if string(got) != mainParameters {
		t.Fatalf("omitted system pool changed main's parameters:\ngot  %s\nwant %s", got, mainParameters)
	}
}

func TestProvisionSystemPoolParameters(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pool  SystemPoolConfig
		size  bool
		count bool
	}{
		{"defaults", SystemPoolConfig{VMSize: DefaultSystemVMSize, Count: 1}, false, false},
		{"size", SystemPoolConfig{VMSize: "Standard_D8s_v6", Count: 1}, true, false},
		{"count", SystemPoolConfig{VMSize: DefaultSystemVMSize, Count: 3}, false, true},
		{"both", SystemPoolConfig{VMSize: "Standard_D8s_v6", Count: 3}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			c.AKS.SystemPool = tc.pool
			r := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "false"},
				reply{contains: "deployment sub what-if", output: `{"status":"Succeeded","changes":[]}`})
			if err := (App{Config: c, Runner: r}).Provision(t.Context(), true); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(c.workspace(), "infra", "parameters.json"))
			if err != nil {
				t.Fatal(err)
			}
			params := map[string]any{
				"location": c.Azure.Location, "resourceGroupName": c.Azure.ResourceGroup,
				"installationId": c.Installation.ID, "clusterName": c.AKS.ClusterName,
				"kubernetesVersion": c.AKS.KubernetesVersion, "workerVmSize": c.AKS.WorkerPool.VMSize,
				"workerCount": c.AKS.WorkerPool.Count, "apiAuthorizedCidrs": c.AKS.APIAuthorizedCIDRs,
				"workerOsDiskType": c.AKS.WorkerPool.osDiskType(), "workerOsDiskSizeGB": c.AKS.WorkerPool.osDiskSizeGB(),
				"workerMaxPods": c.AKS.WorkerPool.MaxPods, "assetPublisherObjectId": c.Azure.AssetPublisherObjectID,
				"assetPublisherPrincipalType": c.publisherPrincipalType(), "snapshotReaderObjectId": c.Azure.SnapshotReaderObjectID,
				"snapshotReaderPrincipalType": c.readerPrincipalType(), "registryOnly": false,
			}
			if tc.size {
				params["systemVmSize"] = tc.pool.VMSize
			}
			if tc.count {
				params["systemCount"] = tc.pool.Count
			}
			wrapped := map[string]any{}
			for key, value := range params {
				wrapped[key] = map[string]any{"value": value}
			}
			want, err := json.Marshal(map[string]any{
				"$schema":        "https://schema.management.azure.com/schemas/2019-04-01/deploymentParameters.json#",
				"contentVersion": "1.0.0.0", "parameters": wrapped,
			})
			if err != nil || !bytes.Equal(data, want) {
				t.Fatalf("parameters changed unexpectedly:\ngot  %s\nwant %s\nerror %v", data, want, err)
			}
		})
	}
}

func TestConfiguredSourceDoesNotInheritDefaultTag(t *testing.T) {
	c := testConfig(t)
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	other := strings.Repeat("c", 40)
	data = []byte(strings.Replace(string(data), `"tag":"`+DefaultTag+`","commit":"`+DefaultCommit+`"`, `"commit":"`+other+`"`, 1))
	path := filepath.Join(t.TempDir(), "installation.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Substrate.Source != (Source{Repository: DefaultRepository, Commit: other}) {
		t.Fatalf("a configured commit inherited the default release tag: %+v", loaded.Substrate.Source)
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
		"SETUP_CSI=both", "STORAGE_CLASS=other",
	)
	got := cleanEnvironment(input)
	if !slices.Equal(got, preserved) {
		t.Fatalf("inherited conflicting settings: %v", got)
	}
}
