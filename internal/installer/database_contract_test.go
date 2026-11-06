//go:build integration

package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
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

func TestHorizonBicepAPIContract(t *testing.T) {
	command := exec.CommandContext(t.Context(), "az", "bicep", "build", "--file", "../../infra/horizondb.bicep", "--stdout")
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	output, err := command.Output()
	if err != nil {
		t.Fatalf("compiling HorizonDB: %v\n%s", err, diagnostics.String())
	}
	var template struct {
		Parameters map[string]struct {
			Type          string
			DefaultValue  any
			AllowedValues []string
		}
		Resources []struct {
			Type, APIVersion, Name string
			Properties             json.RawMessage
			DependsOn              []string
		}
		Outputs map[string]struct{ Value string }
	}
	if err := json.Unmarshal(output, &template); err != nil {
		t.Fatal(err)
	}
	dependsOn := func(deps []string, kind, name string) bool {
		for _, dep := range deps {
			if strings.Contains(dep, "resourceId('"+kind+"'") && strings.Contains(dep, name) {
				return true
			}
		}
		return false
	}
	found := map[string]bool{}
	for _, resource := range template.Resources {
		if resource.APIVersion != horizonAPIVersion {
			t.Fatalf("unexpected HorizonDB API: %+v", resource)
		}
		var properties any
		if err := json.Unmarshal(resource.Properties, &properties); err != nil {
			t.Fatal(err)
		}
		p, _ := properties.(object)
		switch {
		case resource.Type == "Microsoft.HorizonDb/clusters":
			found["cluster"] = true
			// Backup settings stay omitted: some regions reject configurable retention.
			if !reflect.DeepEqual(p, object{"postgreSqlVersion": "17", "network": object{"publicAccess": "Enabled"}}) {
				t.Fatalf("cluster profile changed: %v", p)
			}
		case resource.Type == "Microsoft.HorizonDb/clusters/pools":
			found["pool"] = true
			group, _ := p["parameterGroup"].(object)
			if !strings.Contains(resource.Name, "'default')") ||
				!reflect.DeepEqual(p["computeModel"], object{"type": "Provisioned", "computeProcessor": "Intel", "computeGeneration": float64(1), "vCores": float64(2)}) ||
				p["nodeCount"] != float64(1) || !reflect.DeepEqual(p["highAvailability"], object{"mode": "Disabled"}) ||
				!strings.Contains(fmt.Sprint(group["parameterGroupResourceId"]), "subscriptionResourceId('Microsoft.HorizonDb/locations/parameterGroups', parameters('location'), parameters('parameterGroupName'))") {
				t.Fatalf("pool profile or parameter group changed: %s %v", resource.Name, p)
			}
		case resource.Type == "Microsoft.HorizonDb/clusters/authentications" && strings.Contains(resource.Name, "'passwordMethod'"):
			found["password"] = true
			// administratorLogin is write-once, so only Create may send it.
			expression, _ := properties.(string)
			if !dependsOn(resource.DependsOn, "Microsoft.HorizonDb/clusters/pools", "'default'") ||
				!strings.Contains(expression, "createObject('state', 'Enabled', 'administratorLoginPassword', parameters('adminPassword'))") ||
				!strings.Contains(expression, "if(equals(parameters('createMode'), 'Create'), createObject('administratorLogin', 'substrateadmin'), createObject())") {
				t.Fatalf("password authentication must follow the pool and send the login only on Create: %v %s", resource.DependsOn, expression)
			}
		case resource.Type == "Microsoft.HorizonDb/clusters/authentications" && strings.Contains(resource.Name, "'microsoftEntra'"):
			found["entra"] = true
			if !dependsOn(resource.DependsOn, "Microsoft.HorizonDb/clusters/authentications", "'passwordMethod'") ||
				!reflect.DeepEqual(p, object{"state": "Disabled"}) {
				t.Fatalf("Entra authentication must stay disabled after password setup: %v %v", resource.DependsOn, p)
			}
		case resource.Type == "Microsoft.HorizonDb/clusters/pools/firewallRules":
			found["firewall"] = true
			if !strings.Contains(resource.Name, "'default', 'aks-egress'") ||
				!dependsOn(resource.DependsOn, "Microsoft.HorizonDb/clusters/authentications", "'microsoftEntra'") {
				t.Fatalf("firewall must target the default pool after authentication: %s %v", resource.Name, resource.DependsOn)
			}
			if p["startIpAddress"] != "[parameters('egressIP')]" || p["endIpAddress"] != p["startIpAddress"] {
				t.Fatal("HorizonDB firewall widened beyond the AKS egress IP")
			}
		default:
			t.Fatalf("unexpected deployed HorizonDB resource: %s %s", resource.Type, resource.Name)
		}
	}
	mode := template.Parameters["createMode"]
	if mode.DefaultValue != "Create" || !reflect.DeepEqual(mode.AllowedValues, []string{"Create", "Update"}) ||
		!strings.EqualFold(template.Parameters["adminPassword"].Type, "secureString") ||
		template.Parameters["parameterGroupName"].DefaultValue != horizonParameterGroup {
		t.Fatal("standalone module lost its lifecycle, secure password or checked parameter group parameters")
	}
	host := template.Outputs["host"].Value
	if len(found) != 5 || !strings.Contains(host, "resourceId('Microsoft.HorizonDb/clusters/endpoints'") ||
		!strings.Contains(host, ".fullyQualifiedDomainName") {
		t.Fatalf("missing API resources or read/write endpoint host: %v; %+v", found, template.Outputs)
	}
}

func TestHorizonInventoryPreservesForeignClusters(t *testing.T) {
	c, database := managedTestConfig(t, DatabaseHorizon)
	inventory, err := json.Marshal([]object{{
		"id": database.ResourceID, "location": c.DatabaseLocation(),
		"tags": object{ownershipTag: "another-installation"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	r := script(t, reply{contains: "resource list --resource-type Microsoft.HorizonDb/clusters", output: string(inventory)})
	if _, err := (App{Config: c, Runner: r}).horizonCreateMode(t.Context()); err == nil {
		t.Fatal("foreign cluster in the target group must block deployment")
	}
	var query string
	for index, argument := range r.calls[0].Args {
		if argument == "--query" {
			query = r.calls[0].Args[index+1]
		}
	}
	output, err := exec.CommandContext(t.Context(), "az", "version", "--query",
		"`"+string(inventory)+"` | "+query, "--output", "json").Output()
	if err != nil {
		t.Fatalf("evaluating actual inventory projection: %v", err)
	}
	var projected []object
	if err := json.Unmarshal(output, &projected); err != nil {
		t.Fatal(err)
	}
	if len(projected) != 1 || projected[0]["id"] != database.ResourceID {
		t.Fatal("Azure CLI inventory query hid a foreign cluster and could misclassify it as absent")
	}
}

func TestHorizonStandaloneParameterExamples(t *testing.T) {
	const subscription = "00000000-0000-0000-0000-000000000001"
	for _, example := range []struct{ file, modeParameter, secretParameter, secretName string }{
		{"horizondb.bicepparam", "horizonCreateMode", "databaseCredentials", "database-credentials"},
		{"horizondb-leaf.bicepparam", "createMode", "adminPassword", "database-admin-password"},
	} {
		for _, mode := range []string{"Create", "Update"} {
			t.Run(example.file+"/"+mode, func(t *testing.T) {
				command := exec.CommandContext(t.Context(), "az", "bicep", "build-params",
					"--file", "../../infra/examples/"+example.file, "--stdout")
				command.Env = append(os.Environ(),
					"AZURE_SUBSCRIPTION_ID="+subscription,
					"SUBSTRATE_RESOURCE_GROUP=standalone-example",
					"SUBSTRATE_CLUSTER_NAME=standalone-example",
					"SUBSTRATE_INSTALLATION_ID=00000000-0000-0000-0000-000000000002",
					"SUBSTRATE_API_CIDR=203.0.113.8/32",
					"SUBSTRATE_OPERATOR_OBJECT_ID=00000000-0000-0000-0000-000000000003",
					"SUBSTRATE_DATABASE_VAULT_NAME=example-install-vault",
					"SUBSTRATE_DATABASE_EGRESS_IP=203.0.113.9",
					"SUBSTRATE_INPUT_VAULT_GROUP=example-inputs",
					"SUBSTRATE_INPUT_VAULT_NAME=example-input-vault",
					"HORIZON_CREATE_MODE="+mode)
				var diagnostics bytes.Buffer
				command.Stderr = &diagnostics
				output, err := command.Output()
				if err != nil {
					t.Fatalf("compiling standalone parameters: %v\n%s", err, &diagnostics)
				}
				var built struct{ ParametersJSON, TemplateJSON string }
				if err := json.Unmarshal(output, &built); err != nil {
					t.Fatal(err)
				}
				var parameters struct{ Parameters map[string]map[string]any }
				if err := json.Unmarshal([]byte(built.ParametersJSON), &parameters); err != nil {
					t.Fatal(err)
				}
				if parameters.Parameters[example.modeParameter]["value"] != mode {
					t.Fatal("standalone example lost the caller-selected lifecycle operation")
				}
				wantReference := object{"reference": object{
					"keyVault":   object{"id": "/subscriptions/" + subscription + "/resourceGroups/example-inputs/providers/Microsoft.KeyVault/vaults/example-input-vault"},
					"secretName": example.secretName,
				}}
				if !reflect.DeepEqual(parameters.Parameters[example.secretParameter], map[string]any(wantReference)) {
					t.Fatal("standalone example must resolve credentials through an ARM Key Vault reference, not a plaintext value")
				}
				var template struct {
					Parameters map[string]struct {
						DefaultValue  any
						AllowedValues []string
					}
				}
				if err := json.Unmarshal([]byte(built.TemplateJSON), &template); err != nil {
					t.Fatal(err)
				}
				contract := template.Parameters[example.modeParameter]
				if contract.DefaultValue != "Create" || !reflect.DeepEqual(contract.AllowedValues, []string{"Create", "Update"}) {
					t.Fatal("entrypoint lost the explicit, constrained Create/Update contract")
				}
			})
		}
	}
}

func TestHorizonBicepCreateModeWiring(t *testing.T) {
	command := exec.CommandContext(t.Context(), "az", "bicep", "build", "--file", "../../infra/main.bicep", "--stdout")
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	output, err := command.Output()
	if err != nil {
		t.Fatalf("compiling HorizonDB parameter forwarding: %v\n%s", err, diagnostics.String())
	}
	type resource struct {
		Type       string
		Properties json.RawMessage
	}
	type properties struct {
		Parameters map[string]any
		Template   struct{ Resources []resource }
	}
	var root struct{ Resources []resource }
	if err := json.Unmarshal(output, &root); err != nil {
		t.Fatal(err)
	}
	for _, module := range root.Resources {
		if module.Type != "Microsoft.Resources/deployments" {
			continue
		}
		var moduleProperties properties
		if err := json.Unmarshal(module.Properties, &moduleProperties); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(moduleProperties.Parameters["horizonCreateMode"], object{"value": "[parameters('horizonCreateMode')]"}) {
			continue
		}
		for _, child := range moduleProperties.Template.Resources {
			if child.Type != "Microsoft.Resources/deployments" {
				continue
			}
			var childProperties properties
			if err := json.Unmarshal(child.Properties, &childProperties); err != nil {
				t.Fatal(err)
			}
			if reflect.DeepEqual(childProperties.Parameters["createMode"], object{"value": "[parameters('horizonCreateMode')]"}) {
				return
			}
		}
	}
	t.Fatal("caller-selected HorizonDB Create/Update mode is not forwarded through both deployment modules")
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
