//go:build integration

package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Azure/substrate-aks/deploy"
	"go.yaml.in/yaml/v3"
)

func TestInfrastructureDeploymentHistorySelection(t *testing.T) {
	c := testConfig(t)
	outputs, err := json.Marshal(testInfrastructure())
	if err != nil {
		t.Fatal(err)
	}
	r := script(t, reply{contains: "group exists", output: "true"}, ownerReply(c),
		reply{contains: "deployment sub list", output: string(outputs)})
	if _, err := (App{Config: c, Runner: r}).infrastructure(t.Context()); err != nil {
		t.Fatal(err)
	}
	var query string
	for index, arg := range r.calls[2].Args {
		if arg == "--query" {
			query = r.calls[2].Args[index+1]
		}
	}
	deployment := func(name, state, timestamp, value string) object {
		return object{"name": name, "properties": object{
			"provisioningState": state, "timestamp": timestamp,
			"outputs": object{"infrastructure": object{"value": value}},
		}}
	}
	for _, scenario := range []string{"legacy", "successful retry", "failed retry", "no success"} {
		t.Run(scenario, func(t *testing.T) {
			history := []object{
				deployment(c.deploymentName()+"-vault", "Succeeded", "2026-10-27T16:09:00Z", "vault"),
				deployment(c.deploymentName()+"-registry", "Succeeded", "2026-10-27T16:09:00Z", "registry"),
				deployment("another-installation-run-123", "Succeeded", "2026-10-27T16:09:00Z", "foreign"),
			}
			var want any
			if scenario != "no success" {
				history = append(history, deployment(c.deploymentName(), "Succeeded", "2026-10-27T16:00:00Z", "legacy"))
				want = "legacy"
			}
			if scenario == "successful retry" || scenario == "failed retry" {
				history = append(history, deployment(c.deploymentName()+"-run-abcdef123456", "Succeeded", "2026-10-27T16:02:00Z", "updated"))
				want = "updated"
			}
			if scenario == "failed retry" || scenario == "no success" {
				history = append(history, deployment(c.deploymentName()+"-run-fedcba123456", "Failed", "2026-10-27T16:05:00Z", "failed"))
			}
			data, err := json.Marshal(history)
			if err != nil {
				t.Fatal(err)
			}
			// Exercise the real CLI query engine without authentication or Azure writes.
			output, err := exec.CommandContext(t.Context(), "az", "version", "--query", "`"+string(data)+"` | "+query, "--output", "json").Output()
			if err != nil {
				t.Fatalf("evaluating infrastructure history query: %v", err)
			}
			var got any
			if len(output) != 0 {
				if err := json.Unmarshal(output, &got); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("deployment selection = %v, want %v", got, want)
			}
			output, err = exec.CommandContext(t.Context(), "az", "version", "--query",
				"`"+string(data)+"` | "+c.infrastructureDeploymentFilter()+".name", "--output", "json").Output()
			if err != nil {
				t.Fatalf("evaluating submitted-deployment credential guard: %v", err)
			}
			var submitted []string
			if err := json.Unmarshal(output, &submitted); err != nil || len(submitted) == 0 {
				t.Fatalf("submitted infrastructure attempts disappeared from the credential guard: %s: %v", output, err)
			}
			for _, name := range submitted {
				if strings.HasSuffix(name, "-vault") || strings.HasSuffix(name, "-registry") || strings.HasPrefix(name, "another-") {
					t.Fatalf("credential guard included a non-infrastructure deployment: %q", name)
				}
			}
		})
	}
}

func TestManagedBicepNetworkGrantScope(t *testing.T) {
	command := exec.CommandContext(t.Context(), "az", "bicep", "build",
		"--file", "../../infra/resources.bicep", "--stdout")
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	output, err := command.Output()
	if err != nil {
		t.Fatalf("compiling managed database networking: %v\n%s", err, diagnostics.String())
	}
	var template struct {
		Resources []struct {
			Type       string
			Scope      string
			Properties json.RawMessage
		}
	}
	if err := json.Unmarshal(output, &template); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, resource := range template.Resources {
		if resource.Type != "Microsoft.Authorization/roleAssignments" {
			continue
		}
		var value any
		if err := json.Unmarshal(resource.Properties, &value); err != nil {
			t.Fatal(err)
		}
		properties, ok := value.(map[string]any)
		if !ok {
			continue
		}
		roleDefinitionID, _ := properties["roleDefinitionId"].(string)
		if strings.Contains(roleDefinitionID, "4d97b98b-1d4f-4787-a291-c67834d212e7") {
			found = true
			if !strings.Contains(resource.Scope, "Microsoft.Network/publicIPAddresses") {
				t.Fatal("cluster Network Contributor grant widened beyond the outbound public IP")
			}
		}
	}
	if !found {
		t.Fatal("cluster identity is missing permission to use its explicit outbound IP")
	}
}

func TestManagedBicepVaultContract(t *testing.T) {
	output, err := exec.CommandContext(t.Context(), "az", "bicep", "build",
		"--file", "../../infra/vault.bicep", "--stdout").CombinedOutput()
	if err != nil {
		t.Fatalf("compiling database Key Vault: %v\n%s", err, output)
	}
	var template struct {
		Resources []struct {
			Type       string
			Scope      string
			Properties struct {
				EnableRbacAuthorization      bool
				EnabledForTemplateDeployment bool
				EnableSoftDelete             bool
				SoftDeleteRetentionInDays    int
				RoleDefinitionID             string
			}
		}
	}
	if err := json.Unmarshal(output, &template); err != nil {
		t.Fatal(err)
	}
	var vault, grant bool
	for _, resource := range template.Resources {
		switch resource.Type {
		case "Microsoft.KeyVault/vaults":
			vault = true
			p := resource.Properties
			if !p.EnableRbacAuthorization || !p.EnabledForTemplateDeployment || !p.EnableSoftDelete || p.SoftDeleteRetentionInDays != 7 {
				t.Fatal("vault lost RBAC, ARM secret reference support, or bounded soft-delete retention")
			}
		case "Microsoft.Authorization/roleAssignments":
			grant = true
			if !strings.Contains(resource.Scope, "Microsoft.KeyVault/vaults") ||
				!strings.Contains(resource.Properties.RoleDefinitionID, "b86a8fe4-44ce-4948-aee5-eccb2c155cd7") {
				t.Fatal("database secret permissions widened beyond the vault or changed role")
			}
		}
	}
	if !vault || !grant {
		t.Fatal("database vault or scoped operator grant is missing")
	}
	main, err := exec.CommandContext(t.Context(), "az", "bicep", "build",
		"--file", "../../infra/main.bicep", "--stdout").Output()
	if err != nil {
		t.Fatalf("compiling main template: %v", err)
	}
	var root struct {
		Resources []struct {
			Name       string
			Properties struct {
				Parameters map[string]json.RawMessage
			}
		}
	}
	if err := json.Unmarshal(main, &root); err != nil {
		t.Fatal(err)
	}
	forwarded := false
	for _, module := range root.Resources {
		if !strings.Contains(module.Name, "substrate-vault-") {
			continue
		}
		forwarded = true
		for name, want := range map[string]string{
			"operatorObjectId":      `{"value":"[parameters('operatorObjectId')]"}`,
			"operatorPrincipalType": `{"value":"[parameters('operatorPrincipalType')]"}`,
		} {
			var compact bytes.Buffer
			if err := json.Compact(&compact, module.Properties.Parameters[name]); err != nil || compact.String() != want {
				t.Fatalf("database vault access must follow the AKS operator, not the upload identity: %s = %s", name, compact.String())
			}
		}
	}
	if !forwarded {
		t.Fatal("main template no longer deploys the database vault module")
	}
}

func TestPinnedDatabaseBootstrapScript(t *testing.T) {
	root := pinnedContractRoot(t)
	data, err := deploy.DatabaseBootstrap()
	if err != nil {
		t.Fatal(err)
	}
	var job struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Image   string
						Command []string
						Args    []string
					}
				}
			}
		}
	}
	if err := yaml.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	container := job.Spec.Template.Spec.Containers[0]
	name := fmt.Sprintf("substrate-db-bootstrap-%d", time.Now().UnixNano())
	command := exec.CommandContext(t.Context(), "docker", "run", "--detach", "--name", name,
		"--env=POSTGRES_USER=substrateadmin", "--env=POSTGRES_PASSWORD=bootstrap-test-admin",
		container.Image)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("starting isolated bootstrap test database: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if output, err := exec.CommandContext(ctx, "docker", "rm", "--force", name).CombinedOutput(); err != nil {
			t.Errorf("removing isolated bootstrap test container: %v\n%s", err, output)
		}
	})
	deadline := time.Now().Add(time.Minute)
	for {
		if exec.CommandContext(t.Context(), "docker", "exec", name, "pg_isready", "--host=127.0.0.1", "--username=substrateadmin").Run() == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated PostgreSQL did not become ready")
		}
		time.Sleep(time.Second)
	}
	for attempt := range 2 {
		args := []string{"run", "--rm", "--read-only", "--user=65534:65534", "--network=container:" + name,
			"--mount=type=bind,src=" + filepath.Join(root, "pkg/postgressetup") + ",dst=/bootstrap,readonly",
			"--env=PGHOST=127.0.0.1", "--env=PGUSER=substrateadmin", "--env=PGPASSWORD=bootstrap-test-admin",
			"--env=OWNER_PASSWORD=bootstrap-test-owner", "--env=READWRITE_PASSWORD=bootstrap-test-runtime",
			// TLS is exercised against both real Azure services; this test isolates the Job's SQL/shell behavior.
			"--env=PGSSLMODE=disable", "--env=PGCONNECT_TIMEOUT=20", "--entrypoint=" + container.Command[0],
			container.Image}
		args = append(args, container.Command[1:]...)
		args = append(args, container.Args...)
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("bootstrap attempt %d failed: %v\n%s", attempt+1, err, output)
		}
		for _, secret := range []string{"bootstrap-test-admin", "bootstrap-test-owner", "bootstrap-test-runtime"} {
			if strings.Contains(string(output), secret) {
				t.Fatal("bootstrap leaked credentials in its output")
			}
		}
	}
	command = exec.CommandContext(t.Context(), "docker", "exec", name, "psql", "--no-psqlrc",
		"--username=substrateadmin", "--dbname=substrate", "--tuples-only", "--no-align",
		"--command=SELECT pg_get_userbyid(nspowner), has_schema_privilege('substrate_readwrite', 'substrate', 'USAGE'), has_schema_privilege('substrate_readwrite', 'substrate', 'CREATE') FROM pg_namespace WHERE nspname = 'substrate'")
	output, err = command.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "substrate_owner|t|f" {
		t.Fatalf("bootstrap did not establish a dedicated database and separated schema roles: %v\n%s", err, output)
	}
}

func TestManagedBicepDatabaseSKU(t *testing.T) {
	type template struct {
		Parameters map[string]struct {
			DefaultValue  any
			AllowedValues []string
		}
		Resources []struct {
			Type, Name string
			// Expressions can replace any of these objects, so decode them where needed.
			Tags, SKU, Properties json.RawMessage
		}
	}
	compiled := map[string]template{}
	compile := func(name string) template {
		t.Helper()
		if cached, ok := compiled[name]; ok {
			return cached
		}
		output, err := exec.CommandContext(t.Context(), "az", "bicep", "build", "--file", "../../infra/"+name, "--stdout").Output()
		if err != nil {
			t.Fatalf("compiling %s: %v", name, err)
		}
		var result template
		if err := json.Unmarshal(output, &result); err != nil {
			t.Fatal(err)
		}
		compiled[name] = result
		return result
	}
	tiers := []string{"Burstable", "GeneralPurpose", "MemoryOptimized"}
	// Direct deployers get the installer's default SKU and a constrained tier from every entrypoint.
	for name, prefix := range map[string]string{"main.bicep": "databaseSku", "resources.bicep": "databaseSku", "flexible-server.bicep": "sku"} {
		p := compile(name).Parameters
		if p[prefix+"Name"].DefaultValue != DefaultDatabaseSKU.Name || p[prefix+"Tier"].DefaultValue != DefaultDatabaseSKU.Tier ||
			!reflect.DeepEqual(p[prefix+"Tier"].AllowedValues, tiers) {
			t.Fatalf("%s SKU parameters differ from the installer default or tiers: %+v %+v", name, p[prefix+"Name"], p[prefix+"Tier"])
		}
	}
	forwards := func(name, module string, want map[string]string) {
		t.Helper()
		for _, resource := range compile(name).Resources {
			if resource.Type != "Microsoft.Resources/deployments" || !strings.Contains(resource.Name, module) {
				continue
			}
			var properties struct{ Parameters map[string]json.RawMessage }
			if err := json.Unmarshal(resource.Properties, &properties); err != nil {
				t.Fatalf("%s module parameters: %v", name, err)
			}
			parameters := properties.Parameters
			for parameter, value := range want {
				var got struct{ Value any }
				if err := json.Unmarshal(parameters[parameter], &got); err != nil || got.Value != value {
					t.Fatalf("%s does not forward %s to %s: %s", name, value, parameter, parameters[parameter])
				}
			}
			return
		}
		t.Fatalf("%s has no %s module", name, module)
	}
	forwards("main.bicep", "substrate-aks-", map[string]string{
		"databaseSkuName": "[parameters('databaseSkuName')]", "databaseSkuTier": "[parameters('databaseSkuTier')]"})
	forwards("resources.bicep", "postgres-flexible-", map[string]string{
		"skuName": "[parameters('databaseSkuName')]", "skuTier": "[parameters('databaseSkuTier')]"})
	server := false
	for _, resource := range compile("flexible-server.bicep").Resources {
		var sku map[string]string
		if resource.Type == "Microsoft.DBforPostgreSQL/flexibleServers" && json.Unmarshal(resource.SKU, &sku) == nil {
			server = reflect.DeepEqual(sku, map[string]string{"name": "[parameters('skuName')]", "tier": "[parameters('skuTier')]"})
		}
	}
	if !server {
		t.Fatal("the server does not deploy the SKU parameters")
	}
	// The installer rejects SKU changes by comparing this group tag; both group deployments must write it.
	for name, want := range map[string]string{
		"main.bicep":           "[if(equals(parameters('databaseBackend'), 'bundled'), '', parameters('databaseSkuName'))]",
		"database-vault.bicep": "[parameters('databaseSkuName')]",
	} {
		tagged := false
		for _, resource := range compile(name).Resources {
			var tags map[string]string
			if resource.Type == "Microsoft.Resources/resourceGroups" && json.Unmarshal(resource.Tags, &tags) == nil {
				tagged = tags["substrate-aks-database-sku"] == want
			}
		}
		if !tagged {
			t.Fatalf("%s does not record the database SKU on the resource group", name)
		}
	}
}
