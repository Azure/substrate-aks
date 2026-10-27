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

func TestHorizonBicepAPIContract(t *testing.T) {
	command := exec.CommandContext(t.Context(), "az", "bicep", "build", "--file", "../../infra/horizondb.bicep", "--stdout")
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	output, err := command.Output()
	if err != nil {
		t.Fatalf("compiling HorizonDB: %v\n%s", err, diagnostics.String())
	}
	var template struct {
		Resources []struct {
			Type, APIVersion, Name string
			Properties             map[string]any
			DependsOn              []string
		}
		Outputs map[string]struct{ Value string }
	}
	if err := json.Unmarshal(output, &template); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, resource := range template.Resources {
		if resource.APIVersion != "2026-10-01-preview" {
			t.Fatalf("HorizonDB resource retained a legacy API: %+v", resource)
		}
		p := resource.Properties
		switch resource.Type {
		case "Microsoft.HorizonDb/clusters":
			found["cluster"] = true
			for _, obsolete := range []string{"createMode", "creationMode", "version", "vCores", "processorType", "replicaCount", "zonePlacementPolicy", "authConfig", "administratorLoginPassword"} {
				if _, exists := p[obsolete]; exists {
					t.Fatalf("cluster still sends a creation-only or legacy field: %s", obsolete)
				}
			}
			if p["postgreSqlVersion"] != "17" || !reflect.DeepEqual(p["network"], object{"publicAccess": "Enabled"}) ||
				!reflect.DeepEqual(p["backup"], object{"geoRedundancy": "Disabled", "retentionDays": float64(7)}) {
				t.Fatalf("cluster lost PostgreSQL version, explicit public networking or local backups: %v", p)
			}
		case "Microsoft.HorizonDb/clusters/pools":
			found["pool"] = true
			if !reflect.DeepEqual(p["computeModel"], object{"type": "Provisioned", "computeProcessor": "Intel", "computeGeneration": float64(1), "vCores": float64(2)}) ||
				p["nodeCount"] != float64(1) || !reflect.DeepEqual(p["highAvailability"], object{"mode": "Disabled"}) {
				t.Fatalf("pool profile changed: %v", p)
			}
			group, ok := p["parameterGroup"].(map[string]any)
			if !ok || !strings.Contains(fmt.Sprint(group["parameterGroupResourceId"]), "default-postgresql-17-provisioned") || group["applyMethod"] != "Immediate" {
				t.Fatal("pool is missing its required service-provided default parameter group")
			}
		case "Microsoft.HorizonDb/clusters/authentications":
			if strings.Contains(resource.Name, "passwordMethod") {
				found["password"] = true
				if p["state"] != "Enabled" || p["administratorLogin"] != "substrateadmin" ||
					p["administratorLoginPassword"] != "[parameters('adminPassword')]" {
					t.Fatal("password authentication lost secure, stable administrator credentials")
				}
			} else if strings.Contains(resource.Name, "microsoftEntra") {
				found["entra"] = true
				if p["state"] != "Disabled" {
					t.Fatal("Entra authentication enabled without renewable client support")
				}
			}
		case "Microsoft.HorizonDb/clusters/pools/firewallRules":
			found["firewall"] = true
			if p["startIpAddress"] != "[parameters('egressIP')]" || p["endIpAddress"] != p["startIpAddress"] {
				t.Fatal("HorizonDB firewall widened beyond the AKS egress IP")
			}
		default:
			t.Fatalf("unexpected deployed HorizonDB resource: %s", resource.Type)
		}
	}
	if len(found) != 5 || !strings.Contains(template.Outputs["host"].Value, "Microsoft.HorizonDb/clusters/endpoints") ||
		!strings.Contains(template.Outputs["host"].Value, ".fullyQualifiedDomainName") {
		t.Fatalf("missing API resources or cluster-scoped connection endpoint: %v; %+v", found, template.Outputs)
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
		var properties struct{ RoleDefinitionID string }
		if err := json.Unmarshal(resource.Properties, &properties); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(properties.RoleDefinitionID, "4d97b98b-1d4f-4787-a291-c67834d212e7") {
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
