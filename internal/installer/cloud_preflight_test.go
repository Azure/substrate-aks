package installer

import (
	"fmt"
	"strings"
	"testing"
)

const allCloudPermissions = `{"value":[{"actions":["*"],"notActions":[]}]}`
const availableAKS = `{"values":[{"version":"1.37","patchVersions":{"1.37.2":{}}}]}`

func skuReply(size string) reply {
	return reply{contains: "vm list-skus --location eastus2 --resource-type virtualMachines", output: fmt.Sprintf(
		`[{"name":%q,"family":"standardDSv3Family","capabilities":[{"name":"vCPUs","value":"4"}],"restrictions":[{"type":"Zone","values":["eastus2"],"restrictionInfo":{"zones":["3"]}}]}]`, size)}
}

func cloudReplies(c Config) []reply {
	return []reply{
		accountReply(c), targetReply(), {contains: "group exists", output: "false"},
		{contains: "/providers/Microsoft.Authorization/permissions?", output: allCloudPermissions},
		{contains: "aks get-versions --location eastus2", output: availableAKS},
		skuReply("Standard_D4s_v3"),
		{contains: "vm list-usage --location eastus2", output: `[{"name":{"value":"cores"},"currentValue":"16","limit":"24"},{"name":{"value":"standardDSv3Family"},"currentValue":16,"limit":24}]`},
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
		{"SKU unavailable", `[]`, "not available", 5},
		{"SKU location restricted", `[{ "name":"Standard_D4s_v3","restrictions":[{"type":"Location","values":["eastus2"]}]}]`, "restricted", 5},
		{"regional quota insufficient", `[{"name":{"value":"cores"},"currentValue":16,"limit":23},{"name":{"value":"standardDSv3Family"},"currentValue":16,"limit":24}]`, "insufficient cores", 6},
		{"family quota insufficient", `[{"name":{"value":"cores"},"currentValue":16,"limit":24},{"name":{"value":"standardDSv3Family"},"currentValue":16,"limit":23}]`, "insufficient standarddsv3family", 6},
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
					reply{contains: "vm list-usage", output: `[{"name":{"value":"cores"},"currentValue":"8","limit":"12"},{"name":{"value":"standardDSv3Family"},"currentValue":"8","limit":"12"}]`})
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
	c.AKS.WorkerPool.VMSize = "Standard_D8s_v3"
	sku := skuReply(c.AKS.WorkerPool.VMSize)
	sku.output = strings.Replace(sku.output, `"value":"4"`, `"value":"8"`, 1)
	replies := []reply{accountReply(c), targetReply(), {contains: "group exists", output: "true"}, ownerReply(c),
		clusterReply(old), operatorRecordReply(old),
		{contains: "/subscriptions/" + c.Azure.SubscriptionID + "/providers/Microsoft.Authorization/permissions?", output: allCloudPermissions},
		{contains: "/resourceGroups/" + c.Azure.ResourceGroup + "/providers/Microsoft.Authorization/permissions?", output: allCloudPermissions},
		{contains: "aks get-versions", output: availableAKS}, sku,
		{contains: "vm list-usage", output: `[{"name":{"value":"cores"},"currentValue":8,"limit":15},{"name":{"value":"standardDSv3Family"},"currentValue":8,"limit":15}]`}}
	if err := (App{Config: c, Runner: script(t, replies...)}).CloudPreflight(t.Context()); err == nil || !strings.Contains(err.Error(), "8 additional required") {
		t.Fatalf("resizing incorrectly reused the old pool's CPU quota: %v", err)
	}
}

func TestCloudPreflightLoadsBothPoolSKUsOnce(t *testing.T) {
	c := testConfig(t)
	c.AKS.WorkerPool.VMSize = "Standard_D8s_v3"
	replies := cloudReplies(c)
	worker := skuReply(c.AKS.WorkerPool.VMSize)
	worker.output = strings.Replace(worker.output, `"value":"4"`, `"value":"8"`, 1)
	replies[5].output = strings.TrimSuffix(replies[5].output, "]") + "," + strings.TrimPrefix(worker.output, "[")
	replies[5].effect = func(command Command) {
		args := strings.Join(command.Args, " ")
		if !strings.Contains(args, "name=='Standard_D4s_v3'") || !strings.Contains(args, "name=='Standard_D8s_v3'") {
			t.Fatalf("regional catalog query omitted one of the pool SKUs: %s", args)
		}
	}
	err := (App{Config: c, Runner: script(t, replies...)}).CloudPreflight(t.Context())
	if err == nil || !strings.Contains(err.Error(), "12 additional required") {
		t.Fatalf("did not account for both pool SKUs from one lookup: %v", err)
	}
}
