package installer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

func stringPointer(s string) *string { return &s }

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

func TestConfigSizingAndSnapshotDelays(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
		valid  bool
	}{
		{"unset", func(*Config) {}, true},
		{"valid delays", func(c *Config) { c.Substrate.Atelet = &AteletConfig{stringPointer("10s"), stringPointer("30s")} }, true},
		{"zero upload", func(c *Config) { c.Substrate.Atelet = &AteletConfig{SnapshotUploadDelay: stringPointer("0s")} }, false},
		{"zero detach", func(c *Config) { c.Substrate.Atelet = &AteletConfig{SnapshotDetachDelay: stringPointer("0s")} }, false},
		{"negative", func(c *Config) { c.Substrate.Atelet = &AteletConfig{SnapshotUploadDelay: stringPointer("-1s")} }, false},
		{"invalid", func(c *Config) { c.Substrate.Atelet = &AteletConfig{SnapshotUploadDelay: stringPointer("wrong")} }, false},
		{"empty", func(c *Config) { c.Substrate.Atelet = &AteletConfig{SnapshotUploadDelay: stringPointer("")} }, false},
		{"reversed", func(c *Config) { c.Substrate.Atelet = &AteletConfig{stringPointer("1m"), stringPointer("30s")} }, false},
		{"upload exceeds default detach", func(c *Config) { c.Substrate.Atelet = &AteletConfig{SnapshotUploadDelay: stringPointer("31m")} }, false},
		{"detach below default upload", func(c *Config) { c.Substrate.Atelet = &AteletConfig{SnapshotDetachDelay: stringPointer("30s")} }, false},
		{"upload below default detach", func(c *Config) { c.Substrate.Atelet = &AteletConfig{SnapshotUploadDelay: stringPointer("30s")} }, true},
		{"detach above default upload", func(c *Config) { c.Substrate.Atelet = &AteletConfig{SnapshotDetachDelay: stringPointer("6m")} }, true},
		{"custom pin with one delay", func(c *Config) {
			c.Substrate.Source.Commit = strings.Repeat("a", 40)
			c.Substrate.Atelet = &AteletConfig{SnapshotUploadDelay: stringPointer("1s")}
		}, false},
		{"custom pin with detach only", func(c *Config) {
			c.Substrate.Source.Commit = strings.Repeat("a", 40)
			c.Substrate.Atelet = &AteletConfig{SnapshotDetachDelay: stringPointer("6m")}
		}, false},
		{"custom pin with neither delay", func(c *Config) {
			c.Substrate.Source.Commit = strings.Repeat("a", 40)
			c.Substrate.Atelet = &AteletConfig{}
		}, true},
		{"custom pin with both delays", func(c *Config) {
			c.Substrate.Source.Commit = strings.Repeat("a", 40)
			c.Substrate.Atelet = &AteletConfig{stringPointer("1s"), stringPointer("2s")}
		}, true},
		{"zero replicas", func(c *Config) { c.Substrate.Sizing = &SizingConfig{APIServer: &APIServerSizing{Replicas: new(int)}} }, false},
		{"zero pool", func(c *Config) {
			c.Substrate.Sizing = &SizingConfig{APIServer: &APIServerSizing{PostgresPoolMaxConns: new(int)}}
		}, false},
		{"bad quantity", func(c *Config) {
			c.Substrate.Sizing = &SizingConfig{APIServer: &APIServerSizing{Resources: &ResourceRequirements{Requests: map[string]string{"cpu": "one"}}}}
		}, false},
		{"negative quantity", func(c *Config) {
			c.Substrate.Sizing = &SizingConfig{APIServer: &APIServerSizing{Resources: &ResourceRequirements{Requests: map[string]string{"memory": "-1Mi"}}}}
		}, false},
		{"valid quantity", func(c *Config) {
			c.Substrate.Sizing = &SizingConfig{APIServer: &APIServerSizing{Resources: &ResourceRequirements{Requests: map[string]string{"memory": "1Gi", "cpu": "250m"}}}}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			tc.change(&c)
			if err := c.Validate(); (err == nil) != tc.valid {
				t.Fatalf("Validate() = %v; want valid %t", err, tc.valid)
			}
		})
	}
}

func TestConfigBundledConnectionBudget(t *testing.T) {
	one, three, maxInt := 1, 3, int(^uint(0)>>1)
	pass, fail, defaultPass, defaultFail := 84, 85, 39, 40
	for _, tc := range []struct {
		name        string
		replicas    *int
		pool        *int
		valid       bool
		wantMessage string
	}{
		{"unset", nil, nil, true, ""},
		{"explicit at boundary", &one, &pass, true, ""},
		{"explicit over boundary", &one, &fail, false, "connection budget"},
		{"default replicas at boundary", nil, &defaultPass, true, ""},
		{"default replicas over boundary", nil, &defaultFail, false, "connection budget"},
		{"replicas need pool", &three, nil, false, "explicit postgresPoolMaxConns"},
		{"large replicas", &maxInt, &pass, false, "connection budget"},
		{"large pool", &one, &maxInt, false, "connection budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			c.Substrate.Sizing = &SizingConfig{APIServer: &APIServerSizing{
				Replicas: tc.replicas, PostgresPoolMaxConns: tc.pool,
			}}
			err := c.Validate()
			if (err == nil) != tc.valid || (!tc.valid && !strings.Contains(err.Error(), tc.wantMessage)) {
				t.Fatalf("Validate() = %v; want valid %t and %q", err, tc.valid, tc.wantMessage)
			}
			if !tc.valid {
				r := script(t)
				if err := (App{Config: c, Runner: r}).Install(t.Context(), "ate-system"); err == nil || len(r.calls) != 0 {
					t.Fatalf("invalid budget caused commands: %v, %v", err, r.calls)
				}
			}
		})
	}
	c := testConfig(t)
	c.Substrate.Sizing = &SizingConfig{
		APIServer: &APIServerSizing{Resources: &ResourceRequirements{Requests: map[string]string{"cpu": "1"}}},
		Router:    &RouterSizing{Replicas: &three},
	}
	c.Substrate.Atelet = &AteletConfig{SnapshotUploadDelay: stringPointer("10s")}
	if err := c.Validate(); err != nil {
		t.Fatalf("unrelated sizing must preserve the unset budget exception: %v", err)
	}
	if err := validateConnectionBudget(1, 84, 101); err != nil {
		t.Fatalf("rounded headroom rejected boundary: %v", err)
	}
	if err := validateConnectionBudget(1, 85, 101); err == nil {
		t.Fatal("rounded headroom accepted excess connections")
	}
}

func TestLoadMainEraRecordWithoutSizing(t *testing.T) {
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
	if c.Substrate.Sizing != nil || c.Substrate.Atelet != nil {
		t.Fatalf("old record gained sizing: %+v", c.Substrate)
	}
	plan := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "false"},
		reply{contains: "deployment sub what-if", output: `{"status":"Succeeded","changes":[]}`}, skuReply(SystemVMSize))
	if err := (App{Config: c, Runner: plan}).Provision(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	destroy := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "false"})
	if err := (App{Config: c, Runner: destroy}).Destroy(t.Context(), c.Azure.ResourceGroup); err != nil {
		t.Fatal(err)
	}
	images := testConfig(t).Substrate.Images
	if err := SaveBuildResult(path, c, BuildResult{Images: images, AssetsDir: "/test/assets"}); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadConfig(path)
	if err != nil || saved.Substrate.Sizing != nil || saved.Substrate.Atelet != nil {
		t.Fatalf("image save changed omitted settings: %+v, %v", saved.Substrate, err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"sizing"`) || strings.Contains(string(data), `"atelet"`) {
		t.Fatalf("unset settings were written: %s", data)
	}
}

func TestInstallRejectsInvalidDelayBeforeMutation(t *testing.T) {
	c := testConfig(t)
	c.Substrate.Atelet = &AteletConfig{SnapshotDetachDelay: stringPointer("30s")}
	r := script(t)
	if err := (App{Config: c, Runner: r}).Install(t.Context(), "atelet"); err == nil {
		t.Fatal("invalid delays accepted")
	}
	if len(r.calls) != 0 {
		t.Fatalf("invalid delays caused commands: %v", r.calls)
	}
}

func TestImageSavePreservesSizingAndOmittedDelay(t *testing.T) {
	c := testConfig(t)
	c.Substrate.Images = ImagesConfig{}
	replicas := 3
	pool := 10
	c.Substrate.Sizing = &SizingConfig{APIServer: &APIServerSizing{
		Replicas:             &replicas,
		PostgresPoolMaxConns: &pool,
		Resources: &ResourceRequirements{Requests: map[string]string{
			"cpu": "500m", "memory": "1Gi",
		}},
	}}
	c.Substrate.Atelet = &AteletConfig{SnapshotUploadDelay: stringPointer("10s")}
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
	result := BuildResult{Images: testConfig(t).Substrate.Images, AssetsDir: "/test/assets"}
	if err := SaveBuildResult(path, expected, result); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved.Substrate.Sizing, c.Substrate.Sizing) || !reflect.DeepEqual(saved.Substrate.Atelet, c.Substrate.Atelet) {
		t.Fatalf("image save changed sizing: %+v", saved.Substrate)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"snapshotDetachDelay"`) {
		t.Fatalf("image save persisted an omitted delay: %s", data)
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
