package installer

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
)

const allCloudPermissions = `{"value":[{"actions":["*"],"notActions":[]}]}`
const availableAKS = `{"values":[{"version":"1.37","patchVersions":{"1.37.2":{}}}]}`

func skuReply(size string) reply {
	return reply{contains: "/providers/Microsoft.Compute/skus?", output: fmt.Sprintf(
		`{"value":[{"name":%q,"family":"StandardDsv6Family","capabilities":[{"name":"vCPUs","value":"4"}],"restrictions":[{"type":"Zone","values":["eastus2"],"restrictionInfo":{"zones":["3"]}}]}]}`, size)}
}

func cloudReplies(c Config) []reply {
	return []reply{
		accountReply(c), targetReply(), {contains: "group exists", output: "false"},
		{contains: "/providers/Microsoft.Authorization/permissions?", output: allCloudPermissions},
		{contains: "aks get-versions --location eastus2", output: availableAKS},
		skuReply("Standard_D4s_v6"),
		{contains: "vm list-usage --location eastus2", output: `[{"name":{"value":"cores"},"currentValue":"16","limit":"24"},{"name":{"value":"StandardDsv6Family"},"currentValue":16,"limit":24}]`},
	}
}

func TestCloudPreflightStableBaselinePattern(t *testing.T) {
	for _, tc := range []struct {
		size string
		want bool
	}{
		{"Standard_D4s_v6", true}, {"Standard_D4s_v7", true},
		{"Standard_E8ds_v6", true}, {"Standard_E8ds_v7", true},
		{"Standard_F16s_v6", true}, {"Standard_F16s_v7", true},
		{"Standard_D64-32s_v6", true}, {"Standard_D4_v7", true},
		{"Standard_D4s_v5", false}, {"Standard_D4ps_v6", false}, {"Standard_E4pds_v7", false},
		{"Standard_L8s_v7", false}, {"Standard_D4s_v8", false}, {"standard_D4s_v6", false},
		{"Standard_D4s_v6_extra", false}, {"Standard_Ds_v6", false},
	} {
		t.Run(tc.size, func(t *testing.T) {
			if got := stableBaselineSKU.MatchString(tc.size); got != tc.want {
				t.Fatalf("stable baseline match = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestCloudPreflightMicroVMRestoreRisk(t *testing.T) {
	for _, tc := range []struct {
		name, size     string
		count          int
		optIn, refused bool
		warn           bool
	}{
		{"single older", "Standard_D4s_v5", 1, false, false, false},
		{"single arm", "Standard_D4ps_v6", 1, false, false, false},
		{"single other", "Standard_L8s_v7", 1, false, false, false},
		{"D v6", "Standard_D4s_v6", 2, false, false, false},
		{"D v7", "Standard_D4s_v7", 2, false, false, false},
		{"E v6", "Standard_E8ds_v6", 2, false, false, false},
		{"E v7", "Standard_E8ds_v7", 2, false, false, false},
		{"F v6", "Standard_F16s_v6", 2, false, false, false},
		{"F v7", "Standard_F16s_v7", 2, false, false, false},
		{"older refused", "Standard_D4s_v5", 2, false, true, false},
		{"larger older refused", "Standard_D4s_v5", 100, false, true, false},
		{"arm refused", "Standard_D4ps_v6", 2, false, true, false},
		{"other refused", "Standard_L8s_v7", 2, false, true, false},
		{"older opted in", "Standard_D4s_v5", 2, true, false, true},
		{"arm opted in", "Standard_D4ps_v6", 2, true, false, true},
		{"stable opted in", "Standard_D4s_v6", 2, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			c.AKS.WorkerPool.VMSize, c.AKS.WorkerPool.Count = tc.size, tc.count
			c.AKS.WorkerPool.AllowMixedCPUGenerations = tc.optIn
			replies := cloudReplies(c)
			if tc.refused {
				replies = nil
			} else {
				replies[6].output = `[{"name":{"value":"cores"},"currentValue":0,"limit":1000},{"name":{"value":"StandardDsv6Family"},"currentValue":0,"limit":1000}]`
				if tc.size != SystemVMSize {
					worker := skuReply(tc.size)
					replies[5].output = strings.TrimSuffix(replies[5].output, "]}") + "," + strings.TrimPrefix(worker.output, `{"value":[`)
				}
			}
			var progress []string
			r := script(t, replies...)
			err := (App{Config: c, Runner: r, Progress: func(format string, args ...any) {
				progress = append(progress, fmt.Sprintf(format, args...))
			}}).CloudPreflight(t.Context())
			if tc.refused {
				if err == nil {
					t.Fatal("risky pool was accepted")
				}
				for _, term := range []string{"TSC", "snapshot restore", "_v6", "allowMixedCpuGenerations", tc.size} {
					if !strings.Contains(err.Error(), term) {
						t.Errorf("error %q lacks %q", err, term)
					}
				}
				if len(r.calls) != 0 {
					t.Fatal("refusal happened after cloud calls")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			warnings := strings.Join(progress, "\n")
			if strings.Contains(warnings, "WARNING:") != tc.warn {
				t.Fatalf("unexpected warnings: %s", warnings)
			}
			if tc.warn {
				for _, term := range []string{"TSC", "snapshot restore", "_v6", "allowMixedCpuGenerations", tc.size} {
					if !strings.Contains(warnings, term) {
						t.Errorf("warning %q lacks %q", warnings, term)
					}
				}
			}
		})
	}
}

func TestCloudPreflightChecksReadOnlyCapacityAndPermissions(t *testing.T) {
	for _, tc := range []struct {
		name, output, want string
		index              int
	}{
		{"available including a zone restriction", "", "", -1},
		{"contributor without assignment rights", `{"value":[{"actions":["*"],"notActions":["Microsoft.Authorization/*/Write"]}]}`, "roleAssignments/write", 3},
		{"another grant restores excluded action", `{"value":[{"actions":["*"],"notActions":["Microsoft.Authorization/*/write"]},{"actions":["Microsoft.Authorization/roleAssignments/write"],"notActions":[]}]}`, "", 3},
		{"missing service permission", `{"value":[{"actions":["*"],"notActions":["Microsoft.ContainerService/*"]}]}`, "managedClusters/write", 3},
		{"incomplete permissions", `{"value":[],"nextLink":"https://example.invalid/next"}`, "incomplete", 3},
		{"missing AKS version", `{"values":[{"version":"1.36","patchVersions":{"1.36.2":{}}}]}`, "not listed", 4},
		{"SKU unavailable", `{"value":[]}`, "not available", 5},
		{"SKU location restricted", `{"value":[{"name":"Standard_D4s_v6","restrictions":[{"type":"Location","values":["eastus2"]}]}]}`, "restricted", 5},
		{"SKU catalog incomplete", `{}`, "incomplete VM SKU catalog", 5},
		{"SKU catalog malformed", `{"value":`, "reading regional VM SKU catalog", 5},
		{"regional quota insufficient", `[{"name":{"value":"cores"},"currentValue":16,"limit":23},{"name":{"value":"StandardDsv6Family"},"currentValue":16,"limit":24}]`, "insufficient cores", 6},
		{"family quota insufficient", `[{"name":{"value":"cores"},"currentValue":16,"limit":24},{"name":{"value":"StandardDsv6Family"},"currentValue":16,"limit":23}]`, "insufficient standarddsv6family", 6},
		{"quota missing", `[]`, "did not return CPU quota", 6},
		{"quota invalid", `[{"name":{"value":"cores"},"currentValue":"unknown","limit":"24"}]`, "reading Azure CPU quota", 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			replies := cloudReplies(c)
			if tc.index >= 0 {
				replies[tc.index].output = tc.output
			}
			if tc.want != "" {
				replies = replies[:tc.index+1]
			}
			r := script(t, replies...)
			err := (App{Config: c, Runner: r}).CloudPreflight(t.Context())
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("cloud preflight: %v (want %q)", err, tc.want)
			}
			for _, command := range r.calls {
				if strings.Contains(strings.Join(command.Args, " "), "deployment sub create") {
					t.Fatal("preflight submitted a deployment")
				}
			}
		})
	}
}

func TestCloudPreflightRetryChecksOnlyAdditionalCapacity(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			c := testConfig(t)
			c.AKS.WorkerPool.Count = count
			replies := []reply{accountReply(c), targetReply(), {contains: "group exists", output: "true"}, ownerReply(c),
				clusterReply(c), operatorRecordReply(c),
				{contains: "/subscriptions/" + c.Azure.SubscriptionID + "/providers/Microsoft.Authorization/permissions?", output: `{"value":[{"actions":["Microsoft.Resources/deployments/write"],"notActions":[]}]}`},
				{contains: "/resourceGroups/" + c.Azure.ResourceGroup + "/providers/Microsoft.Authorization/permissions?", output: allCloudPermissions},
				{contains: "aks get-versions", output: availableAKS}}
			if count == 2 {
				replies = append(replies, skuReply(c.AKS.WorkerPool.VMSize),
					reply{contains: "vm list-usage", output: `[{"name":{"value":"cores"},"currentValue":"8","limit":"12"},{"name":{"value":"StandardDsv6Family"},"currentValue":"8","limit":"12"}]`})
			}
			if err := (App{Config: c, Runner: script(t, replies...)}).CloudPreflight(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCloudPreflightRequiresExactPatchWhenConfigured(t *testing.T) {
	for _, patch := range []string{"1.37.2", "1.37.3"} {
		t.Run(patch, func(t *testing.T) {
			c := testConfig(t)
			c.AKS.KubernetesVersion = patch
			replies := cloudReplies(c)
			if patch == "1.37.3" {
				replies = replies[:5]
			}
			err := (App{Config: c, Runner: script(t, replies...)}).CloudPreflight(t.Context())
			if patch == "1.37.2" && err != nil || patch == "1.37.3" && (err == nil || !strings.Contains(err.Error(), "AKS 1.37.3 is not listed")) {
				t.Fatalf("exact patch availability: %v", err)
			}
		})
	}
}

func TestCloudPreflightDoesNotCreditDifferentWorkerSKU(t *testing.T) {
	old := testConfig(t)
	c := old
	c.AKS.WorkerPool.VMSize = "Standard_D8s_v6"
	sku := skuReply(c.AKS.WorkerPool.VMSize)
	sku.output = strings.Replace(sku.output, `"value":"4"`, `"value":"8"`, 1)
	replies := []reply{accountReply(c), targetReply(), {contains: "group exists", output: "true"}, ownerReply(c),
		clusterReply(old), operatorRecordReply(old),
		{contains: "/subscriptions/" + c.Azure.SubscriptionID + "/providers/Microsoft.Authorization/permissions?", output: allCloudPermissions},
		{contains: "/resourceGroups/" + c.Azure.ResourceGroup + "/providers/Microsoft.Authorization/permissions?", output: allCloudPermissions},
		{contains: "aks get-versions", output: availableAKS}, sku,
		{contains: "vm list-usage", output: `[{"name":{"value":"cores"},"currentValue":8,"limit":15},{"name":{"value":"StandardDsv6Family"},"currentValue":8,"limit":15}]`}}
	if err := (App{Config: c, Runner: script(t, replies...)}).CloudPreflight(t.Context()); err == nil || !strings.Contains(err.Error(), "8 additional required") {
		t.Fatalf("resizing incorrectly reused the old pool's CPU quota: %v", err)
	}
}

func TestCloudPreflightLoadsBothPoolSKUsOnce(t *testing.T) {
	c := testConfig(t)
	c.AKS.WorkerPool.VMSize = "Standard_D8s_v6"
	replies := cloudReplies(c)
	worker := skuReply(c.AKS.WorkerPool.VMSize)
	worker.output = strings.Replace(worker.output, `"value":"4"`, `"value":"8"`, 1)
	replies[5].output = strings.TrimSuffix(replies[5].output, "]}") + "," + strings.TrimPrefix(worker.output, `{"value":[`)
	replies[5].effect = func(command Command) {
		args := strings.Join(command.Args, " ")
		if !strings.Contains(args, "name=='Standard_D4s_v6'") || !strings.Contains(args, "name=='Standard_D8s_v6'") {
			t.Fatalf("regional catalog query omitted one of the pool SKUs: %s", args)
		}
		index := slices.Index(command.Args, "--url")
		if index < 0 || index+1 >= len(command.Args) {
			t.Fatalf("SKU lookup did not use the regional REST endpoint: %s", args)
		}
		endpoint, err := url.Parse(command.Args[index+1])
		if err != nil || endpoint.Query().Get("$filter") != "location eq 'eastus2'" ||
			endpoint.Query().Get("api-version") != "2021-07-01" ||
			!strings.Contains(args, "resourceType=='virtualMachines'") || !strings.Contains(args, "nextLink:nextLink") {
			t.Fatalf("SKU lookup lost server-side region filtering or pagination: %s", args)
		}
	}

	err := (App{Config: c, Runner: script(t, replies...)}).CloudPreflight(t.Context())
	if err == nil || !strings.Contains(err.Error(), "12 additional required") {
		t.Fatalf("did not account for both pool SKUs from one lookup: %v", err)
	}
}

func TestRegionalSKUCatalogFollowsPagesAndPropagatesFailures(t *testing.T) {
	c := testConfig(t)
	next := "https://management.azure.com/subscriptions/" + c.Azure.SubscriptionID + "/providers/Microsoft.Compute/skus?continuation=next"
	page := skuReply("Standard_D4s_v6")
	page.output = strings.TrimSuffix(page.output, "}") + fmt.Sprintf(`,"nextLink":%q}`, next)
	want := errors.New("second SKU page unavailable")
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%t", fail), func(t *testing.T) {
			worker := skuReply("Standard_D8s_v6")
			worker.contains = next
			if fail {
				worker.err = want
			}
			data, err := (App{Config: c, Runner: script(t, page, worker)}).regionalSKUCatalog(t.Context())
			if fail {
				if !errors.Is(err, want) {
					t.Fatalf("lost pagination failure: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, size := range []string{"Standard_D4s_v6", "Standard_D8s_v6"} {
				if _, _, err := (App{Config: c}).cloudSKU(data, size); err != nil {
					t.Fatalf("lost SKU from a catalog page: %v", err)
				}
			}
		})
	}
}

func TestRegionalSKUCatalogRejectsInvalidOrRepeatedPages(t *testing.T) {
	c := testConfig(t)
	endpoint := "https://management.azure.com/subscriptions/" + c.Azure.SubscriptionID + "/providers/Microsoft.Compute/skus"
	for _, next := range []string{
		"https://example.invalid/skus",
		strings.Replace(endpoint, "https:", "http:", 1),
		strings.Replace(endpoint, c.Azure.SubscriptionID, "another-subscription", 1),
		endpoint + "?continuation=loop",
	} {
		t.Run(next, func(t *testing.T) {
			page := skuReply("Standard_D4s_v6")
			page.output = strings.TrimSuffix(page.output, "}") + fmt.Sprintf(`,"nextLink":%q}`, next)
			replies := []reply{page}
			if strings.Contains(next, "continuation=loop") {
				replies = append(replies, page)
			}
			_, err := (App{Config: c, Runner: script(t, replies...)}).regionalSKUCatalog(t.Context())
			if err == nil || (!strings.Contains(err.Error(), "pagination URL") && !strings.Contains(err.Error(), "repeated")) {
				t.Fatalf("accepted unsafe or looping pagination URL: %v", err)
			}
		})
	}
}

func TestEphemeralOSDiskMustFitLocalStorage(t *testing.T) {
	catalog := []byte(`[
		{"name":"Standard_D8ds_v7","capabilities":[{"name":"EphemeralOSDiskSupported","value":"True"},
			{"name":"SupportedEphemeralOSDiskPlacements","value":"NvmeDisk"},{"name":"NvmeDiskSizeInMiB","value":"450560"},
			{"name":"MaxResourceVolumeMB","value":"0"}]},
		{"name":"Standard_D8s_v7","capabilities":[{"name":"EphemeralOSDiskSupported","value":"False"}]},
		{"name":"Standard_L80as_v4","capabilities":[{"name":"EphemeralOSDiskSupported","value":"True"},
			{"name":"SupportedEphemeralOSDiskPlacements","value":"NvmeDisk"},{"name":"NvmeDiskSizeInMiB","value":"20971520"}]},
		{"name":"Standard_D8ds_v5","capabilities":[{"name":"EphemeralOSDiskSupported","value":"True"},
			{"name":"SupportedEphemeralOSDiskPlacements","value":"ResourceDisk,CacheDisk"},{"name":"CachedDiskBytes","value":"214748364800"},
			{"name":"MaxResourceVolumeMB","value":"307200"}]}
	]`)
	for _, tc := range []struct {
		size string
		gb   int
		ok   bool
	}{
		{"Standard_D8ds_v7", 128, true}, {"Standard_D8ds_v7", 440, true}, {"Standard_D8ds_v7", 441, false},
		{"Standard_D8s_v7", 128, false},
		{"Standard_D8ds_v5", 300, true}, {"Standard_D8ds_v5", 301, false},
		{"Standard_L80as_v4", 2040, true}, {"Standard_L80as_v4", 2048, false},
		{"Standard_Missing", 128, false},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.size, tc.gb), func(t *testing.T) {
			if err := ephemeralOSDiskFits(catalog, tc.size, tc.gb); (err == nil) != tc.ok {
				t.Fatalf("ephemeral OS disk check: %v", err)
			}
		})
	}
}
